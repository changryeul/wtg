// mci-price-krx — KRX 파생 시세 내부 허브 (트랙2, C sise 피드 흡수).
// KRX 멀티캐스트(원 TR) 수신 → pkg/krx 파싱 → 종목 상태 → /dev/shm/mfsise(MFSISE_T)
// 적재. yuanta trn AP 는 기존 libmfsise(l_mfread)로 무수정 read. (mci-price 의 KRX 판)
//
// linux 전용 (mmap /dev/shm). docs/krx-sise-design.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/winwaysystems/wtg/internal/pricekrx"
	"github.com/winwaysystems/wtg/pkg/krxshm"
)

func main() {
	group := flag.String("mcast-group", "227.10.20.10", "KRX 멀티캐스트 그룹 IP")
	ports := flag.String("mcast-ports", "60641,60642,60643,60631,60632", "포트 (콤마)")
	iface := flag.String("mcast-iface", "", "수신 인터페이스 (비면 기본)")
	shmPath := flag.String("shm", krxshm.ShmPath, "파생 MFSISE_T SHM 경로")
	shmBond := flag.String("shm-bond", krxshm.BondShmPath, "채권 MBSISE_T SHM 경로")
	listen := flag.String("listen", ":8088", "HTTP health/stats listen (비면 비활성)")
	rcvbuf := flag.Int("rcvbuf", 32*1024*1024, "소켓 수신버퍼 바이트")
	statsIv := flag.Duration("stats", 30*time.Second, "stats 로그 주기")
	logLevel := flag.String("log-level", "info", "debug/info/warn/error")
	flag.Parse()

	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(*logLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})).With("svc", "mci-price-krx")

	m, err := krxshm.Open(*shmPath)
	if err != nil {
		log.Error("파생 SHM open 실패", "error", err)
		os.Exit(1)
	}
	defer m.Close()
	bm, err := krxshm.OpenBond(*shmBond)
	if err != nil {
		log.Error("채권 SHM open 실패", "error", err)
		os.Exit(1)
	}
	defer bm.Close()
	log.Info("SHM 적재 준비", "fut", *shmPath, "bond", *shmBond,
		"futSize", krxshm.ShmSize, "bondSize", krxshm.BondShmSize)

	hub := pricekrx.New(m.Writer, bm.BondWriter)
	var st stats

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	var ifi *net.Interface
	if *iface != "" {
		if ifi, err = net.InterfaceByName(*iface); err != nil {
			log.Error("iface 조회 실패", "error", err)
			os.Exit(1)
		}
	}
	grp := net.ParseIP(*group)
	if grp == nil || grp.To4() == nil {
		log.Error("mcast-group IPv4 아님", "group", *group)
		os.Exit(1)
	}
	for _, p := range splitCSV(*ports) {
		port, err := strconv.Atoi(p)
		if err != nil {
			log.Error("포트 파싱 실패", "port", p)
			os.Exit(1)
		}
		conn, err := net.ListenMulticastUDP("udp4", ifi, &net.UDPAddr{IP: grp, Port: port})
		if err != nil {
			log.Error("mcast join 실패", "port", p, "error", err)
			os.Exit(1)
		}
		if *rcvbuf > 0 {
			_ = conn.SetReadBuffer(*rcvbuf)
		}
		go readLoop(ctx, conn, hub, &st, log)
	}
	log.Info("KRX 멀티캐스트 수신 시작", "group", *group, "ports", *ports, "iface", *iface)

	if *listen != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			fm, fq, bm, bq := hub.Stats()
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "ok packets=%d applied=%d futMasters=%d futQuotes=%d bondMasters=%d bondQuotes=%d\n",
				st.packets.Load(), st.applied.Load(), fm, fq, bm, bq)
		})
		mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
			fm, fq, bm, bq := hub.Stats()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"packets":%d,"applied":%d,"unknown":%d,"futMasters":%d,"futQuotes":%d,"bondMasters":%d,"bondQuotes":%d}`+"\n",
				st.packets.Load(), st.applied.Load(), st.unknown.Load(), fm, fq, bm, bq)
		})
		hs := &http.Server{Addr: *listen, Handler: mux}
		go func() {
			if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("http listen", "error", err)
			}
		}()
		go func() { <-ctx.Done(); _ = hs.Close() }()
		log.Info("health/stats HTTP", "listen", *listen)
	}

	go func() {
		t := time.NewTicker(*statsIv)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fm, fq, bm, bq := hub.Stats()
				log.Info("stats", "packets", st.packets.Load(), "applied", st.applied.Load(),
					"unknown", st.unknown.Load(),
					"futMasters", fm, "futQuotes", fq, "bondMasters", bm, "bondQuotes", bq)
			}
		}
	}()

	<-ctx.Done()
	_ = m.Sync()
	log.Info("mci-price-krx 종료")
}

type stats struct{ packets, applied, unknown atomic.Uint64 }

func readLoop(ctx context.Context, conn *net.UDPConn, hub *pricekrx.Hub, st *stats, log *slog.Logger) {
	defer conn.Close()
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
			continue
		}
		st.packets.Add(1)
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		_, applied, e := hub.Ingest(pkt)
		if e != nil || !applied {
			st.unknown.Add(1)
			continue
		}
		st.applied.Add(1)
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}
