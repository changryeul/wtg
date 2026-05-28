// quote-forwarder — mds 의 UDP FIX 시세를 mymq broker 로 broadcast 발행한다.
//
// 흐름:
//
//	replay_smb2/kmb2/ebs2  ─UDP→  forwarder  ─FCCast/SubBroadcast→  broker
//	                                                                    ↓
//	                                                                mci-push (representative receiver)
//	                                                                    ↓
//	                                                                ws → 브라우저
//
// FIX 4.4 의 Market Data Snapshot(35=W) / Incremental Refresh(35=X) 를 파싱해서
// 의미 있는 JSON envelope (symbol/bid/ask/trade px·qty) 으로 변환한 뒤 broadcast.
// 원본 FIX wire 가 필요하면 --include-fix=true 로 envelope 에 같이 박는다.
//
// broadcast prefix 의 LogonID 를 빈 값으로 두므로 mci-push 의 dispatcher 가
// 전체 ws 사용자에게 fan-out 한다 (시세는 사용자별 구독이 아닌 공통 채널).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/quote"
)

// 운영 가시성용 카운터 — feed 별 라벨로 분리.
//
// received_total       : UDP 수신 (forwarder 가 받은 모든 패킷)
// published_total      : broker publish 성공
// publish_errors_total : broker publish 실패 (heartbeat 끊김, write fail 등)
// parse_errors_total   : FIX 파싱 결과 msgtype=unknown (raw 그대로 박힘)
// bytes_total          : UDP 페이로드 바이트 합 (대역폭 추정)
//
// /metrics 으로 노출되어 Prometheus scrape 가능.
type quoteMetrics struct {
	received      *prometheus.CounterVec
	published     *prometheus.CounterVec
	publishErrors *prometheus.CounterVec
	parseErrors   *prometheus.CounterVec
	bytes         *prometheus.CounterVec
	rcvBuf        *prometheus.GaugeVec
	batchSize     *prometheus.HistogramVec
	queueDrops    *prometheus.CounterVec
}

func newQuoteMetrics(reg *metrics.Registry) *quoteMetrics {
	m := &quoteMetrics{
		received: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "quote_forwarder_received_total", Help: "UDP 패킷 수신 횟수"},
			[]string{"feed"},
		),
		published: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "quote_forwarder_published_total", Help: "broker publish 성공 횟수"},
			[]string{"feed"},
		),
		publishErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "quote_forwarder_publish_errors_total", Help: "broker publish 실패 횟수"},
			[]string{"feed"},
		),
		parseErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "quote_forwarder_parse_errors_total", Help: "FIX 파싱 실패 횟수"},
			[]string{"feed"},
		),
		bytes: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "quote_forwarder_bytes_total", Help: "UDP 페이로드 바이트 합"},
			[]string{"feed"},
		),
		rcvBuf: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Name: "quote_forwarder_udp_rcvbuf_bytes", Help: "UDP SO_RCVBUF 실제 적용된 크기"},
			[]string{"feed"},
		),
		batchSize: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "quote_forwarder_batch_size",
				Help:    "broker publish 1회당 envelope 수 (batch publish)",
				Buckets: []float64{1, 2, 4, 8, 16, 32, 64},
			},
			[]string{"feed"},
		),
		queueDrops: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "quote_forwarder_queue_drops_total", Help: "reader → worker 채널 가득 시 명시적 drop"},
			[]string{"feed"},
		),
	}
	for _, c := range []prometheus.Collector{m.received, m.published, m.publishErrors, m.parseErrors, m.bytes, m.rcvBuf, m.batchSize, m.queueDrops} {
		_ = reg.Register(c)
	}
	return m
}

var (
	totalReceived atomic.Uint64
	totalPubOK    atomic.Uint64
	totalPubErr   atomic.Uint64
	startedAt     = time.Now()
)

func main() {
	var (
		listen      = flag.String("listen", "127.0.0.1:30044", "단일 mode 의 UDP listen 주소 (--multi 가 비어있을 때 사용)")
		feed        = flag.String("feed", "SMB", "단일 mode 의 거래소 라벨")
		multi       = flag.String("multi", "", "다중 feed — 형식: SMB:30044,KMB:30045,EBS:30046,REUT:30051. 비어있으면 --listen/--feed 사용")
		bindAddr    = flag.String("bind", "127.0.0.1", "--multi 사용 시 모든 listener 가 bind 할 주소")
		brokerHost  = flag.String("broker-host", "127.0.0.1", "mymqd 호스트")
		brokerPort  = flag.Int("broker-port", 11217, "mymqd 포트")
		appl        = flag.String("appl", "quote-fwd", "broker appl 이름")
		instance    = flag.Int("instance", 1, "appl 인스턴스 번호")
		includeFix   = flag.Bool("include-fix", false, "true 면 envelope 에 raw FIX(가독화) 도 같이 박는다")
		metricsAddr  = flag.String("metrics", "", "Prometheus metrics + /stats HTTP listen 주소 (예: 127.0.0.1:9091). 비면 비활성")
		udpRcvBuf    = flag.Int("udp-rcvbuf", 4*1024*1024, "UDP socket SO_RCVBUF (bytes). kernel 한계(macOS kern.ipc.maxsockbuf 기본 8MB)를 넘으면 silently clamp. 0 이면 OS default.")
		batchMax     = flag.Int("batch-max", 14, "한 broker message 에 묶을 envelope 최대 개수 (1=batch 비활성). pushdata.msgb 1512B 한계 — typical envelope 105B × 14 = 1485B (마진 27B). over-size 시 split fallback 으로 안전.")
		batchTimeout = flag.Duration("batch-timeout", 10*time.Millisecond, "batch 가 batch-max 에 도달 못해도 이 시간 후 강제 flush")
		feedBuffer   = flag.Int("feed-buffer", 8192, "feed 별 reader → worker 채널 버퍼 크기. 가득 차면 reader 가 명시적 drop (kernel silent drop 회피, queue_drop 카운터로 가시화).")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	feeds, err := parseFeedSpec(*multi, *listen, *feed, *bindAddr)
	if err != nil {
		logger.Error("--multi 파싱 실패", slog.Any("err", err))
		os.Exit(2)
	}
	logger.Info("quote-forwarder 부팅",
		slog.Int("feeds", len(feeds)),
		slog.String("broker", fmt.Sprintf("%s:%d", *brokerHost, *brokerPort)),
	)
	for _, f := range feeds {
		logger.Info("feed", slog.String("label", f.Label), slog.String("listen", f.Addr))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// metrics 초기화 + HTTP listener (옵션)
	reg := metrics.NewRegistry()
	qm := newQuoteMetrics(reg)
	if *metricsAddr != "" {
		go startMetricsServer(ctx, logger, *metricsAddr, reg, feeds, *brokerHost, *brokerPort)
	}

	// 각 feed 마다 (UDP listener + 독립 broker connection) goroutine.
	//
	// 왜 feed 별 독립 connection 인가:
	//   pkg/mymq.Client.writeFrame 가 writeMu 로 직렬화한다 (단일 connection
	//   thread-safety 보장). 모든 feed 가 1개 connection 을 공유하면 4 goroutine
	//   이 한 writeMu 를 경쟁 → broker Send 가 사실상 single-threaded ~4k tick/s
	//   ceiling. feed 별 connection 으로 분리하면 writeMu 도 분리되어 broker
	//   write 가 N 배 parallel.
	//
	// Instance 번호는 *instance 를 base 로 feed 인덱스를 더해서 broker 측에서
	// 4 connection 이 서로 다른 ApplName ("quote-fwd-01" ~ "quote-fwd-04") 으로
	// 보이도록 한다.
	for i, f := range feeds {
		i, f := i, f
		addr, err := net.ResolveUDPAddr("udp", f.Addr)
		if err != nil {
			logger.Error("listen 주소 파싱 실패", slog.String("feed", f.Label), slog.Any("err", err))
			os.Exit(1)
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			logger.Error("UDP listen 실패", slog.String("feed", f.Label), slog.Any("err", err))
			os.Exit(1)
		}
		defer conn.Close()
		// SO_RCVBUF 키움 — burst 시세 (수만 tick/sec) 시 kernel UDP 큐 overflow
		// 가 가장 큰 손실 위치 (load-gen 측정 결과). 요청값이 kern.ipc.maxsockbuf
		// 를 넘으면 OS 가 silently clamp 하므로 syscall 로 실제값 확인 후 로그.
		actualBuf := 0
		if *udpRcvBuf > 0 {
			if err := conn.SetReadBuffer(*udpRcvBuf); err != nil {
				logger.Warn("UDP SetReadBuffer 실패", slog.String("feed", f.Label), slog.Any("err", err))
			}
			if got, err := actualReadBuffer(conn); err == nil {
				actualBuf = got
			}
		}
		logger.Info("UDP listen",
			slog.String("feed", f.Label),
			slog.String("addr", f.Addr),
			slog.Int("rcvbuf_req", *udpRcvBuf),
			slog.Int("rcvbuf_actual", actualBuf),
		)
		qm.rcvBuf.WithLabelValues(f.Label).Set(float64(actualBuf))

		// feed별 독립 broker connection — ApplName="<base>-<NN>" 으로 broker
		// 측에서 4 connection 이 별개 client 로 보이게.
		mq, err := mymq.Open(ctx, *brokerHost, *brokerPort, mymq.Options{
			ApplName:         *appl,
			Instance:         *instance + i, // base + feed 인덱스
			Channel:          mymq.ChannelAdmin,
			HandshakeTimeout: 5 * time.Second,
			Logger:           logger,
			Reconnect: &mymq.ReconnectOptions{
				InitialBackoff: 1 * time.Second,
				MaxBackoff:     30 * time.Second,
				BackoffFactor:  2.0,
			},
		})
		if err != nil {
			logger.Error("broker 연결 실패", slog.String("feed", f.Label), slog.Any("err", err))
			os.Exit(1)
		}
		defer mq.Close()
		si := mq.SessionInfo()
		logger.Info("broker 연결 OK",
			slog.String("feed", f.Label),
			slog.Int("instance", *instance+i),
			slog.Int("scid", int(si.ConnectionID)),
		)
		go feedLoop(ctx, logger, mq, conn, f.Label, *includeFix, qm, *batchMax, *batchTimeout, *feedBuffer)
	}

	<-ctx.Done()
	logger.Info("종료")
}

type feedSpec struct {
	Label string `json:"label"`
	Addr  string `json:"addr"` // host:port
}

// parseFeedSpec 는 --multi 형식 (label:port[,label:port,...]) 또는 단일 mode 의
// listen/feed 를 정규화해 feedSpec 슬라이스로 반환.
//
// 항목에 ":port" 만 있으면 bindAddr 로 host 를 채운다. "host:port" 도 허용.
func parseFeedSpec(multi, listen, feed, bindAddr string) ([]feedSpec, error) {
	if multi == "" {
		return []feedSpec{{Label: feed, Addr: listen}}, nil
	}
	out := []feedSpec{}
	for _, raw := range splitCSV(multi) {
		p := splitColon(raw)
		if len(p) < 2 || p[0] == "" || p[len(p)-1] == "" {
			return nil, fmt.Errorf("잘못된 feed 항목: %q (형식: LABEL:PORT 또는 LABEL:HOST:PORT)", raw)
		}
		var label, addr string
		switch len(p) {
		case 2:
			label = p[0]
			addr = bindAddr + ":" + p[1]
		case 3:
			label = p[0]
			addr = p[1] + ":" + p[2]
		default:
			return nil, fmt.Errorf("잘못된 feed 항목: %q", raw)
		}
		out = append(out, feedSpec{Label: label, Addr: addr})
	}
	return out, nil
}

func splitCSV(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func splitColon(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ':' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(c)
	}
	out = append(out, cur)
	return out
}

// feedLoop 은 한 거래소의 UDP packet 을 읽어 broker broadcast 로 발사한다.
//
// 각 단계마다 metrics 카운터를 증가 — Prometheus 로 노출.
// rawPacket — reader → worker 채널의 단위. seq 는 forwarder 통합 카운터.
type rawPacket struct {
	data []byte
	seq  uint64
}

func feedLoop(ctx context.Context, logger *slog.Logger, mq *mymq.Client, conn *net.UDPConn, label string, includeFix bool, qm *quoteMetrics, batchMax int, batchTimeout time.Duration, feedBuffer int) {
	go func() {
		<-ctx.Done()
		_ = conn.SetReadDeadline(time.Now())
	}()
	if batchMax < 1 {
		batchMax = 1
	}
	if feedBuffer < 1 {
		feedBuffer = 1
	}
	batch := make([]quote.JSONEnvelope, 0, batchMax)

	// ── Reader goroutine ──
	//
	// pure ReadFromUDP loop — parse/publish 작업 분리. UDP read syscall 만
	// 빠르게 돌려 kernel buffer 가 차지 않게 한다. 채널 가득 시 명시적 drop
	// (queue_drops 카운터) 으로 kernel silent drop 회피. 채널 capacity 가
	// burst 흡수 buffer 역할.
	pktCh := make(chan rawPacket, feedBuffer)
	go func() {
		buf := make([]byte, 64*1024)
		var seq uint64
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				if ctx.Err() != nil {
					close(pktCh)
					return
				}
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				logger.Warn("UDP read", slog.String("feed", label), slog.Any("err", err))
				continue
			}
			seq++
			totalReceived.Add(1)
			qm.received.WithLabelValues(label).Inc()
			qm.bytes.WithLabelValues(label).Add(float64(n))

			// buf 의 사본 — 다음 read 가 덮어쓰지 않게 (channel 의 수신자가
			// 비동기로 소비). 작은 패킷 (~150B) 이라 alloc 비용 소소.
			pkt := make([]byte, n)
			copy(pkt, buf[:n])
			select {
			case pktCh <- rawPacket{data: pkt, seq: seq}:
			default:
				// 채널 가득 — worker 가 parse/publish 못 따라감. 명시적 drop.
				qm.queueDrops.WithLabelValues(label).Inc()
			}
		}
	}()

	var seq uint64

	// publishOne — batch (또는 sub-batch) 1 회 publish. ErrPushdataPayloadTooLong
	// 만 별도 처리해 caller 가 split 재시도 가능하게 분리 신호.
	publishOne := func(envs []quote.JSONEnvelope) error {
		var wire []byte
		var err error
		if len(envs) == 1 {
			wire, err = quote.EncodePushdataV1(envs[0])
		} else {
			wire, err = quote.EncodePushdataBatch(envs)
		}
		if err != nil {
			return err
		}
		if err := publishBroadcast(mq, wire); err != nil {
			return err
		}
		totalPubOK.Add(uint64(len(envs)))
		qm.published.WithLabelValues(label).Add(float64(len(envs)))
		qm.batchSize.WithLabelValues(label).Observe(float64(len(envs)))
		return nil
	}

	// flush — batch 를 broker 로 보낸다. payload 초과 (envelope 누적 size 가
	// pushdata.msgb 1512B 한계 초과) 면 batch 를 둘로 나눠 재귀 split 발행 —
	// 1-element 까지 내려가도 안 들어가면 drop.
	var flushSplit func(envs []quote.JSONEnvelope)
	flushSplit = func(envs []quote.JSONEnvelope) {
		if len(envs) == 0 {
			return
		}
		err := publishOne(envs)
		if err == nil {
			return
		}
		if err == quote.ErrPushdataPayloadTooLong && len(envs) > 1 {
			// 반으로 split — 평균 envelope size 가 110B 가정 시 거의 발생 안 함
			// (batchMax=10 으로 안전 마진). 실측 envelope 가 크면 폴백 진입.
			mid := len(envs) / 2
			flushSplit(envs[:mid])
			flushSplit(envs[mid:])
			return
		}
		totalPubErr.Add(uint64(len(envs)))
		qm.publishErrors.WithLabelValues(label).Add(float64(len(envs)))
		logger.Warn("publish 실패 — drop", slog.String("feed", label),
			slog.Int("batch_size", len(envs)), slog.Any("err", err))
	}
	flush := func() {
		flushSplit(batch)
		batch = batch[:0]
	}

	// ── Worker loop (현 goroutine) ──
	//
	// pktCh 에서 raw UDP packet 을 받아 parse + batch + publish. batch 가 빈
	// 동안 timer 도 stop — 작은 GC pressure 와 CPU 절약. batch 첫 envelope
	// 시점에 timer 재시작.
	flushTimer := time.NewTimer(time.Hour) // dummy 초기값 — 곧 stop
	if !flushTimer.Stop() {
		<-flushTimer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-flushTimer.C:
			// batch flush 시간 도래.
			flush()
		case pkt, ok := <-pktCh:
			if !ok {
				return
			}
			seq = pkt.seq

			// FIX → v1 envelope (fast path — alloc 1, 125ns vs parseQuote
			// 의 28 alloc 595ns). audit 로그 필요 시는 includeFix 분기에서
			// 별도 parseQuote 호출 — hot path 에선 fastExtractV1 만 사용.
			sym, bid, ask, v1ok := fastExtractV1(pkt.data)
			if !v1ok {
				qm.parseErrors.WithLabelValues(label).Inc()
				continue
			}

			// batch 에 누적. 첫 envelope 면 timer 시작.
			if len(batch) == 0 {
				flushTimer.Reset(batchTimeout)
			}
			batch = append(batch, quote.JSONEnvelope{
				Sym: sym, Bid: bid, Ask: ask,
				TS:  time.Now().UTC(),
				Src: label, Seq: seq,
			})
			if len(batch) >= batchMax {
				if !flushTimer.Stop() {
					select {
					case <-flushTimer.C:
					default:
					}
				}
				flush()
			}

			if seq%1000 == 1 {
				logger.Info("forwarded",
					slog.String("feed", label), slog.Uint64("seq", seq), slog.Int("len", len(pkt.data)),
					slog.String("sym", sym), slog.Float64("bid", bid), slog.Float64("ask", ask),
					slog.Int("batch_max", batchMax),
				)
			}
			if includeFix {
				audit := buildEnvelope(label, seq, pkt.data, true)
				logger.Debug("audit", slog.String("fix_envelope", string(audit)))
			}
		}
	}
}

// fastExtractV1 — single-scan FIX 파서. parseQuote + extractV1 의 hot-path
// 대체. struct/slice allocation 없이 sym/bid/ask 만 추출 (worker 의 처리율
// 향상). audit 로그 (--include-fix) 가 필요한 경우는 호출자가 parseQuote 를
// 별도로 호출.
//
// 알고리즘:
//   - SOH (\x01) 로 필드 경계, '=' 로 tag/value 분리.
//   - tag '55' : sym (envelope 또는 entry level — 첫 등장만 채움)
//   - tag '269': 다음 270 의 entry type ('0'=bid, '1'=ask, 기타=무시)
//   - tag '270': entry type 에 따라 bid/ask 할당 (첫 등장만)
//   - 결과 검증: sym!="" && bid>0 && ask>=bid → ok=true
//
// 한계: 270 만 single string conversion (ParseFloat 가 string 요구). 그 외
// alloc 없음. ParseFloat 의 alloc 비용 (작음) 이 진짜 hot path 면 별도
// byte 기반 parser 필요 — 일단 보수적.
func fastExtractV1(buf []byte) (sym string, bid, ask float64, ok bool) {
	var entryType byte
	start := 0
	for i := 0; i <= len(buf); i++ {
		if i < len(buf) && buf[i] != fixSOH {
			continue
		}
		field := buf[start:i]
		start = i + 1
		eq := bytesIndexByte(field, '=')
		if eq < 1 {
			continue
		}
		tag := field[:eq]
		val := field[eq+1:]
		switch {
		case len(tag) == 2 && tag[0] == '5' && tag[1] == '5':
			if sym == "" {
				sym = string(val)
			}
		case len(tag) == 3 && tag[0] == '2' && tag[1] == '6' && tag[2] == '9':
			if len(val) >= 1 {
				entryType = val[0]
			}
		case len(tag) == 3 && tag[0] == '2' && tag[1] == '7' && tag[2] == '0':
			f, err := strconv.ParseFloat(string(val), 64)
			if err != nil {
				continue
			}
			switch entryType {
			case '0':
				if bid == 0 {
					bid = f
				}
			case '1':
				if ask == 0 {
					ask = f
				}
			}
		}
	}
	if sym != "" && bid > 0 && ask >= bid {
		ok = true
	}
	return
}

// bytesIndexByte — bytes.IndexByte 의 inline wrapper. 컴파일러가 inline 함.
func bytesIndexByte(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// extractV1 은 rich quoteEnvelope 에서 mci-price 가 기대하는 v1 평면 envelope
// (docs/cooker-quote-schema.md) 을 추출한다. ok=false 이면 publish 생략.
//
// 규칙:
//   - 35=W (snapshot) 은 env.Symbol 최상위 + entries 의 bid/ask 가 짝으로 옴
//   - 35=X (incremental) 은 entry 마다 e.Symbol 가짐. 단일 side 만 올 수도 있음
//   - 한 메시지에서 bid 와 ask 둘 다 못 추출하면 v1 envelope 으로 발행 불가.
//     (시세 cache 기반 결합은 forwarder scope 밖 — cooker 가 stateful 처리.)
func extractV1(env quoteEnvelope) (sym string, bid, ask float64, ok bool) {
	for _, e := range env.Entries {
		switch e.Type {
		case "bid":
			if bid == 0 && e.Px > 0 {
				bid = e.Px
				if sym == "" {
					sym = e.Symbol
				}
			}
		case "ask":
			if ask == 0 && e.Px > 0 {
				ask = e.Px
				if sym == "" {
					sym = e.Symbol
				}
			}
		}
		if bid > 0 && ask > 0 {
			break
		}
	}
	if sym == "" {
		sym = env.Symbol
	}
	if sym == "" || bid <= 0 || ask <= 0 || ask < bid {
		return "", 0, 0, false
	}
	return sym, bid, ask, true
}

// ── FIX parsing ──────────────────────────────────────────────────────────

const fixSOH = 0x01

// fixFields 는 FIX wire 를 (tag,val) 슬라이스로 파싱한다 — repeating group 도 flat
// 으로 둬서 단순한 sequential 처리가 가능하게 둠.
func fixFields(buf []byte) [][2]string {
	parts := bytes.Split(buf, []byte{fixSOH})
	out := make([][2]string, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		eq := bytes.IndexByte(p, '=')
		if eq <= 0 {
			continue
		}
		out = append(out, [2]string{string(p[:eq]), string(p[eq+1:])})
	}
	return out
}

// MDEntryType (tag 269) → 사람이 읽을 라벨.
func mdEntryTypeLabel(v string) string {
	switch v {
	case "0":
		return "bid"
	case "1":
		return "ask"
	case "2":
		return "trade"
	case "4":
		return "open"
	case "5":
		return "close"
	case "6":
		return "settle"
	case "7":
		return "high"
	case "8":
		return "low"
	case "B":
		return "vwap"
	default:
		return "type" + v
	}
}

// mdUpdateActionLabel (tag 279) — 35=X 의 entry-level update 동작.
func mdUpdateActionLabel(v string) string {
	switch v {
	case "0":
		return "new"
	case "1":
		return "change"
	case "2":
		return "delete"
	default:
		return v
	}
}

type mdEntry struct {
	Action string  `json:"action,omitempty"` // new/change/delete (35=X)
	Type   string  `json:"type"`             // bid/ask/trade/...
	Symbol string  `json:"symbol,omitempty"` // entry-level symbol (35=X)
	Px     float64 `json:"px,omitempty"`
	Qty    float64 `json:"qty,omitempty"`
}

type quoteEnvelope struct {
	Ts      string    `json:"ts"`
	Feed    string    `json:"feed"`
	Seq     uint64    `json:"seq"`
	MsgType string    `json:"msgtype"`           // snapshot / incremental / unknown
	Symbol  string    `json:"symbol,omitempty"`  // 전체 symbol (35=W)
	Sender  string    `json:"sender,omitempty"`  // 49 SenderCompID
	Target  string    `json:"target,omitempty"`  // 56 TargetCompID
	Entries []mdEntry `json:"entries,omitempty"` // MD entries
	Fix     string    `json:"fix,omitempty"`     // 옵션 — raw FIX (SOH→|)
	Raw     string    `json:"raw,omitempty"`     // 파싱 실패 fallback
}

// parseQuote 는 buf 를 35=W/X Market Data 로 시도하고, 실패 시 raw 만 채운 envelope 반환.
//
// repeating group 처리: tag 269 또는 279 가 등장하면 새 entry 시작 (close-out).
// 나머지 270/271/55 는 현재 entry 에 누적.
func parseQuote(buf []byte) quoteEnvelope {
	fs := fixFields(buf)
	if len(fs) == 0 {
		return quoteEnvelope{MsgType: "unknown", Raw: string(buf)}
	}

	env := quoteEnvelope{}
	var (
		curEntry  *mdEntry
		topSymbol string
		entries   []mdEntry
		msgType   string
	)
	flush := func() {
		if curEntry != nil {
			entries = append(entries, *curEntry)
			curEntry = nil
		}
	}
	atof := func(s string) float64 {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}

	for _, kv := range fs {
		tag, val := kv[0], kv[1]
		switch tag {
		case "35": // MsgType
			switch val {
			case "W":
				msgType = "snapshot"
			case "X":
				msgType = "incremental"
			default:
				msgType = "msgtype-" + val
			}
		case "49":
			env.Sender = val
		case "56":
			env.Target = val
		case "55":
			// snapshot 에선 top-level, incremental 에선 per-entry.
			// 이미 entry 가 있으면 entry 에, 아니면 top-level.
			if curEntry != nil {
				curEntry.Symbol = val
			} else {
				topSymbol = val
			}
		case "279": // MDUpdateAction (incremental 의 entry 시작)
			flush()
			curEntry = &mdEntry{Action: mdUpdateActionLabel(val)}
		case "269": // MDEntryType
			// snapshot 의 entry 는 269 로 시작. incremental 은 279 가 entry 시작.
			if curEntry == nil || curEntry.Type != "" {
				flush()
				curEntry = &mdEntry{}
			}
			curEntry.Type = mdEntryTypeLabel(val)
		case "270":
			if curEntry == nil {
				curEntry = &mdEntry{}
			}
			curEntry.Px = atof(val)
		case "271":
			if curEntry == nil {
				curEntry = &mdEntry{}
			}
			curEntry.Qty = atof(val)
		}
	}
	flush()

	env.MsgType = msgType
	env.Symbol = topSymbol
	env.Entries = entries
	return env
}

// ── envelope 빌드 + publish ─────────────────────────────────────────────

func buildEnvelope(feed string, seq uint64, payload []byte, includeFix bool) []byte {
	env := parseQuote(payload)
	env.Ts = time.Now().UTC().Format(time.RFC3339Nano)
	env.Feed = feed
	env.Seq = seq

	if includeFix || env.MsgType == "unknown" {
		// SOH → '|' 로 가독화한 사본을 박는다.
		buf := make([]byte, len(payload))
		for i, b := range payload {
			if b == fixSOH {
				buf[i] = '|'
			} else {
				buf[i] = b
			}
		}
		env.Fix = string(buf)
	}

	out, _ := json.Marshal(env)
	return out
}

// actualReadBuffer — 현재 socket 에 실제 적용된 SO_RCVBUF 값. SetReadBuffer
// 요청이 kernel 한계로 clamp 됐는지 확인용 (macOS kern.ipc.maxsockbuf).
//
// Linux 는 호출자가 요청한 값을 내부적으로 2배로 두 채 저장하므로 GetsockoptInt
// 가 요청값의 2배를 돌려준다 (유명한 quirk). 검증 시 그것까지 감안.
func actualReadBuffer(conn *net.UDPConn) (int, error) {
	sc, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var size int
	var sockErr error
	ctlErr := sc.Control(func(fd uintptr) {
		size, sockErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	})
	if ctlErr != nil {
		return 0, ctlErr
	}
	return size, sockErr
}

// publishBroadcast — FCCast/SubBroadcast, LogonID 빈값 (전체 ws 사용자 fan-out).
// Exchange 는 BroadcastHeader 에 박는다. FrameInput.Xchg 에 박으면 pkg/mymq
// applyDefaults 가 navi 자동채움(transaction 모드)을 트리거해 broker 가
// publish_packet 대신 message_packet_transfer 로 분기, "Lost reply message"
// 로 drop 한다. broker 는 publish_packet 안에서 broadcast->exchange 필드를
// 보고 receiver list 의 client->xchg 와 매칭한다 (publish.c:103, 223).
func publishBroadcast(mq *mymq.Client, payload []byte) error {
	var hdr mymq.BroadcastHeader
	hdr.Function = byte(mymq.FCCast)
	hdr.SubFunction = byte(mymq.SubBroadcast)
	copy(hdr.Exchange[:], mymq.ExchangePrice)

	body := make([]byte, mymq.BroadcastPrefixSize+len(payload))
	mymq.EncodeBroadcastHeader(body[:mymq.BroadcastPrefixSize], &hdr)
	copy(body[mymq.BroadcastPrefixSize:], payload)

	// Dirf=DirPublish 명시 — applyDefaults 가 보정해주지만, broadcast 의도를
	// 호출 지점에서 분명히. Xchg/Navis 는 비워야 broker 가 broadcast 로 인식.
	return mq.Send(&mymq.FrameInput{
		Func: mymq.FCCast,
		Subc: mymq.SubBroadcast,
		Dirf: mymq.DirPublish,
		Body: body,
	})
}

// ── HTTP metrics / stats ─────────────────────────────────────────────────

// startMetricsServer — Prometheus /metrics + 단순 JSON /stats + /healthz.
//
// /stats 는 사람이 보기 좋은 압축 요약 (totalReceived/PubOK/PubErr + uptime).
func startMetricsServer(ctx context.Context, logger *slog.Logger, addr string,
	reg *metrics.Registry, feeds []feedSpec, brokerHost string, brokerPort int) {

	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	// pprof — metrics endpoint 와 같은 listener 에 부착 (부하 진단용).
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		stat := map[string]any{
			"uptime_sec":      time.Since(startedAt).Seconds(),
			"received_total":  totalReceived.Load(),
			"published_total": totalPubOK.Load(),
			"publish_errors":  totalPubErr.Load(),
			"feeds":           feeds,
			"broker":          fmt.Sprintf("%s:%d", brokerHost, brokerPort),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stat)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 5 * time.Second}
	logger.Info("metrics HTTP listen", slog.String("addr", addr),
		slog.String("metrics", "/metrics"),
		slog.String("stats", "/stats"))

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Warn("metrics server 종료", slog.Any("err", err))
	}
}
