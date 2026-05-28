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
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/mymq"
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
	}
	for _, c := range []prometheus.Collector{m.received, m.published, m.publishErrors, m.parseErrors, m.bytes} {
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
		includeFix  = flag.Bool("include-fix", false, "true 면 envelope 에 raw FIX(가독화) 도 같이 박는다")
		metricsAddr = flag.String("metrics", "", "Prometheus metrics + /stats HTTP listen 주소 (예: 127.0.0.1:9091). 비면 비활성")
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

	mq, err := mymq.Open(ctx, *brokerHost, *brokerPort, mymq.Options{
		ApplName:         *appl,
		Instance:         *instance,
		Channel:          mymq.ChannelAdmin,
		HandshakeTimeout: 5 * time.Second,
		Logger:           logger,
		// 자동 재접속 — heartbeat 타임아웃이나 일시 단절에서도 forwarder 가
		// 살아 남도록. 없으면 단발 끊김 후 모든 publish 가 closed connection.
		Reconnect: &mymq.ReconnectOptions{
			InitialBackoff: 1 * time.Second,
			MaxBackoff:     30 * time.Second,
			BackoffFactor:  2.0,
		},
	})
	if err != nil {
		logger.Error("broker 연결 실패", slog.Any("err", err))
		os.Exit(1)
	}
	defer mq.Close()
	si := mq.SessionInfo()
	logger.Info("broker 연결 OK", slog.Int("scid", int(si.ConnectionID)))

	// metrics 초기화 + HTTP listener (옵션)
	reg := metrics.NewRegistry()
	qm := newQuoteMetrics(reg)
	if *metricsAddr != "" {
		go startMetricsServer(ctx, logger, *metricsAddr, reg, feeds, *brokerHost, *brokerPort)
	}

	// 각 feed 마다 UDP listener goroutine.
	for _, f := range feeds {
		f := f
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
		logger.Info("UDP listen", slog.String("feed", f.Label), slog.String("addr", f.Addr))
		go feedLoop(ctx, logger, mq, conn, f.Label, *includeFix, qm)
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
func feedLoop(ctx context.Context, logger *slog.Logger, mq *mymq.Client, conn *net.UDPConn, label string, includeFix bool, qm *quoteMetrics) {
	go func() {
		<-ctx.Done()
		_ = conn.SetReadDeadline(time.Now())
	}()
	buf := make([]byte, 64*1024)
	var seq uint64
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("UDP read", slog.String("feed", label), slog.Any("err", err))
			continue
		}
		seq++
		totalReceived.Add(1)
		qm.received.WithLabelValues(label).Inc()
		qm.bytes.WithLabelValues(label).Add(float64(n))

		envelope, parsedOK := buildEnvelopeWithStatus(label, seq, buf[:n], includeFix)
		if !parsedOK {
			qm.parseErrors.WithLabelValues(label).Inc()
		}
		if err := publishBroadcast(mq, envelope); err != nil {
			totalPubErr.Add(1)
			qm.publishErrors.WithLabelValues(label).Inc()
			logger.Warn("broker publish 실패", slog.String("feed", label), slog.Any("err", err), slog.String("src", src.String()))
			continue
		}
		totalPubOK.Add(1)
		qm.published.WithLabelValues(label).Inc()
		if seq%100 == 1 {
			logger.Info("forwarded", slog.String("feed", label), slog.Uint64("seq", seq), slog.Int("len", n))
		}
	}
}

// buildEnvelopeWithStatus 는 buildEnvelope 의 wrapper — 파싱 결과의 정상 여부도
// 같이 반환해 카운터 분류에 쓸 수 있게 한다 (msgtype != snapshot/incremental
// 이면 parse 실패로 분류).
func buildEnvelopeWithStatus(feed string, seq uint64, payload []byte, includeFix bool) ([]byte, bool) {
	out := buildEnvelope(feed, seq, payload, includeFix)
	// out 의 msgtype 을 빠르게 확인하기 위해 다시 파싱하지 않고 substring 만.
	// raw 처리에 추가 비용 거의 없음.
	ok := bytes.Contains(out, []byte(`"msgtype":"snapshot"`)) ||
		bytes.Contains(out, []byte(`"msgtype":"incremental"`))
	return out, ok
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
