package price

import (
	"context"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/winwaysystems/wtg/pkg/fixmd"
	"github.com/winwaysystems/wtg/pkg/lpcatalog"
	"github.com/winwaysystems/wtg/pkg/quote"
)

// FXMcastReceiver — OMS 가 LP별 multicast group 으로 재송출하는 FX 시세를 직수신한다
// (KRX 의 mci-price-krx 패턴과 대칭). config(lpcatalog)로 어느 group 을 join 할지,
// 수신 패킷을 어느 LP(Source)로 태깅할지 결정 — **하드코딩 없음**. FIX 35=W 파싱은
// pkg/fixmd 공유. 파싱 결과(avail 포함)를 v1 envelope 로 Server.IngestEnvelopes 에
// feed → 기존 BestConsumer(per-source)/Pricing/Aggregator 파이프라인 그대로.
// docs/order-architecture.md §5a.
type FXMcastReceiver struct {
	srv    *Server
	cat    *lpcatalog.Catalog
	iface  *net.Interface
	rcvBuf int
	logger *slog.Logger

	conns    []*net.UDPConn
	received atomic.Uint64
	rejected atomic.Uint64
	feeds    int
}

// NewFXMcastReceiver — ifaceName 비면 시스템 기본 iface, rcvBuf 0 이면 OS 기본.
func NewFXMcastReceiver(srv *Server, cat *lpcatalog.Catalog, ifaceName string, rcvBuf int, logger *slog.Logger) (*FXMcastReceiver, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var ifi *net.Interface
	if ifaceName != "" {
		var err error
		if ifi, err = net.InterfaceByName(ifaceName); err != nil {
			return nil, err
		}
	}
	return &FXMcastReceiver{srv: srv, cat: cat, iface: ifi, rcvBuf: rcvBuf, logger: logger}, nil
}

// Start — lpcatalog 의 ActiveFeeds() 각 LP group 을 join 하고 reader goroutine 가동.
// join 은 시작 시 카탈로그 snapshot 기준 (LP 추가는 재시작 반영 — FIX counterparty 와 동일).
func (r *FXMcastReceiver) Start(ctx context.Context) error {
	for _, lp := range r.cat.ActiveFeeds() {
		grp := net.ParseIP(lp.Group)
		if grp == nil || grp.To4() == nil {
			r.logger.Warn("FX mcast: group IPv4 아님 — skip", slog.String("lp", lp.Code), slog.String("group", lp.Group))
			continue
		}
		conn, err := net.ListenMulticastUDP("udp4", r.iface, &net.UDPAddr{IP: grp, Port: lp.Port})
		if err != nil {
			r.logger.Warn("FX mcast join 실패 — skip", slog.String("lp", lp.Code),
				slog.String("group", lp.Group), slog.Int("port", lp.Port), slog.Any("error", err))
			continue
		}
		if r.rcvBuf > 0 {
			_ = conn.SetReadBuffer(r.rcvBuf)
		}
		r.conns = append(r.conns, conn)
		r.feeds++
		go r.readLoop(ctx, conn, lp.Code)
		r.logger.Info("FX mcast join", slog.String("lp", lp.Code),
			slog.String("group", lp.Group), slog.Int("port", lp.Port))
	}
	if r.feeds == 0 {
		r.logger.Warn("FX mcast: active LP feed 0 — 수신부 비활성")
	}
	go func() { <-ctx.Done(); r.Close() }()
	return nil
}

// readLoop — 한 LP group 의 datagram 을 읽어 파싱·태깅·인제스트.
func (r *FXMcastReceiver) readLoop(ctx context.Context, conn *net.UDPConn, lpCode string) {
	buf := make([]byte, 65536)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		s, ok := fixmd.ParseSnapshot(buf[:n])
		if !ok {
			r.rejected.Add(1)
			continue
		}
		env := quote.JSONEnvelope{
			Sym: s.Sym, Bid: s.Bid, Ask: s.Ask,
			Src:  lpCode, // group→LP 태깅 (per-source)
			TS:   time.Now().UTC(),
			Last: s.Last, LastQty: s.LastQty,
			BidSize: s.BidSize, AskSize: s.AskSize, // avail
		}
		body, mErr := quote.EncodeJSONEnvelope(env)
		if mErr != nil {
			r.rejected.Add(1)
			continue
		}
		r.srv.IngestEnvelopes(body, &Tick{Received: time.Now()})
		r.received.Add(1)
	}
}

// Close — 모든 conn 종료 (idempotent).
func (r *FXMcastReceiver) Close() {
	for _, c := range r.conns {
		_ = c.Close()
	}
}

// Stats — 진단용 수신/거부/피드 수.
func (r *FXMcastReceiver) Stats() (received, rejected uint64, feeds int) {
	return r.received.Load(), r.rejected.Load(), r.feeds
}
