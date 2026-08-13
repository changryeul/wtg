package krx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

// McastStats 는 수신기 카운터 (진단/메트릭).
type McastStats struct {
	Packets  atomic.Uint64 // 수신 datagram 총합
	Fanout   atomic.Uint64 // Ingest 성공(파싱+fan-out) 총합
	Unknown  atomic.Uint64 // 미지원 TR / 파싱 실패
	ReadErrs atomic.Uint64 // 소켓 read 오류
}

// runMcastSource 는 (트랙2) KRX 멀티캐스트에서 원 TR 을 수신해 Ingest 로 흘린다.
// 포트별로 그룹 join + read 루프 goroutine 을 띄운다. 각 datagram = 원 TR 1건 가정
// (KRX 실시간은 통상 datagram 당 1 TR; 다중이면 후속 길이분할).
func (srv *Server) runMcastSource(ctx context.Context, cfg Config) error {
	group := net.ParseIP(cfg.McastGroup)
	if group == nil || group.To4() == nil {
		return errors.New("krx: mcast-group 이 IPv4 아님: " + cfg.McastGroup)
	}
	var ifi *net.Interface
	if cfg.McastIface != "" {
		var err error
		if ifi, err = net.InterfaceByName(cfg.McastIface); err != nil {
			return errors.New("krx: mcast-iface 조회 실패: " + err.Error())
		}
	}
	ports := splitCodes(cfg.McastPorts)
	if len(ports) == 0 {
		return errors.New("krx: mcast-ports 비어있음")
	}

	srv.logger.Info("KRX 멀티캐스트 수신 시작",
		slog.String("group", cfg.McastGroup), slog.Any("ports", ports),
		slog.String("iface", cfg.McastIface))

	for _, p := range ports {
		port, err := strconv.Atoi(p)
		if err != nil {
			return errors.New("krx: mcast 포트 파싱 실패: " + p)
		}
		conn, err := net.ListenMulticastUDP("udp4", ifi, &net.UDPAddr{IP: group, Port: port})
		if err != nil {
			return errors.New("krx: mcast join 실패 (port " + p + "): " + err.Error())
		}
		if cfg.McastRcvBuf > 0 {
			_ = conn.SetReadBuffer(cfg.McastRcvBuf)
		}
		go srv.mcastReadLoop(ctx, conn, port)
	}
	// 주기 stats 로그
	go srv.mcastStatsLoop(ctx)
	return nil
}

// mcastReadLoop 는 한 포트의 datagram 을 읽어 Ingest.
func (srv *Server) mcastReadLoop(ctx context.Context, conn *net.UDPConn, port int) {
	defer conn.Close()
	buf := make([]byte, 65536)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue // 주기 wakeup — ctx 재확인
			}
			srv.mstats.ReadErrs.Add(1)
			continue
		}
		srv.mstats.Packets.Add(1)
		// datagram 사본을 Ingest (buf 재사용 방지).
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		if _, _, _, err := srv.Ingest(pkt); err != nil {
			srv.mstats.Unknown.Add(1)
			continue
		}
		srv.mstats.Fanout.Add(1)
	}
}

// mcastStatsLoop 는 30초마다 수신 통계를 로그.
func (srv *Server) mcastStatsLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			srv.logger.Info("KRX mcast stats",
				slog.Uint64("packets", srv.mstats.Packets.Load()),
				slog.Uint64("fanout", srv.mstats.Fanout.Load()),
				slog.Uint64("unknown", srv.mstats.Unknown.Load()),
				slog.Uint64("read_errs", srv.mstats.ReadErrs.Load()),
				slog.Int("conns", srv.hub.Count()))
		}
	}
}
