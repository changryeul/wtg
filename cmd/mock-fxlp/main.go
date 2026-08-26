// mock-fxlp — LP별 multicast 재송출 시세 mock (OMS 재송출 경로 대역).
//
// OMS 가 인터뱅크 시세를 수신해 LP별 multicast group 으로 재송출하는 실경로를
// 대신한다. etc/lp.json(lpcatalog) 을 읽어 **active LP 각각의 group:port** 로
// 결정적 FIX 4.4 35=W(top-of-book + 271 avail)를 주기 송신 → mci-price 의
// FXMcastReceiver 가 group→LP(Source)로 태깅 수신 → BestConsumer per-source →
// margin → edge → client 까지 값이 결정적으로 흐르는지 e2e 검증한다.
//
// **하드코딩 없음** — 어떤 LP 를, 어느 group:port 로 보낼지는 전적으로 config.
// LP 코드별로 심볼당 가격을 살짝 어긋나게 흘려 cross/best-of-N 도 관측 가능.
package main

import (
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/winwaysystems/wtg/pkg/lpcatalog"
)

func main() {
	var (
		lpFile   = flag.String("lp-file", "etc/lp.json", "LP 카탈로그(config) 경로")
		symbols  = flag.String("symbols", "USD/KRW,EUR/USD,USD/JPY", "송신 심볼 CSV")
		iface    = flag.String("iface", "", "송신 iface 이름 (빈값=OS 기본)")
		interval = flag.Duration("interval", time.Second, "심볼당 송신 주기")
		once     = flag.Bool("once", false, "1회만 송신 후 종료")
		ttl      = flag.Int("ttl", 1, "multicast TTL")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	items, err := lpcatalog.LoadFile(*lpFile)
	if err != nil {
		logger.Error("LP 카탈로그 로드 실패", slog.String("file", *lpFile), slog.Any("error", err))
		os.Exit(1)
	}
	cat := lpcatalog.NewCatalog()
	cat.Replace(items)
	feeds := cat.ActiveFeeds()
	if len(feeds) == 0 {
		logger.Error("active LP feed 0 — 보낼 대상 없음")
		os.Exit(1)
	}

	// 단일 송신 소켓 → LP별 group:port 로 WriteTo (mat-sise-bridge 패턴).
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		logger.Error("송신 소켓 실패", slog.Any("error", err))
		os.Exit(1)
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	// 같은 호스트의 mci-price 로 loopback 전달 필요.
	if lerr := pc.SetMulticastLoopback(true); lerr != nil {
		logger.Warn("set mcast loopback", slog.Any("error", lerr))
	}
	_ = pc.SetTTL(*ttl)
	if *iface != "" {
		ifi, ierr := net.InterfaceByName(*iface)
		if ierr != nil {
			logger.Error("iface 조회 실패", slog.String("iface", *iface), slog.Any("error", ierr))
			os.Exit(1)
		}
		if serr := pc.SetMulticastInterface(ifi); serr != nil {
			logger.Error("set mcast iface", slog.Any("error", serr))
			os.Exit(1)
		}
	}

	syms := splitCSV(*symbols)

	// LP별 송신 목적지 (group:port).
	type sender struct {
		lp  lpcatalog.LP
		dst *net.UDPAddr
	}
	var senders []sender
	for _, lp := range feeds {
		grp := net.ParseIP(lp.Group)
		if grp == nil || grp.To4() == nil {
			logger.Warn("group IPv4 아님 — skip", slog.String("lp", lp.Code), slog.String("group", lp.Group))
			continue
		}
		senders = append(senders, sender{lp: lp, dst: &net.UDPAddr{IP: grp, Port: lp.Port}})
		logger.Info("LP 송신 준비", slog.String("lp", lp.Code), slog.String("cat", string(lp.Category)),
			slog.String("group", lp.Group), slog.Int("port", lp.Port))
	}
	if len(senders) == 0 {
		logger.Error("송신 가능한 LP 0")
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	tick := 0
	send := func() {
		for _, s := range senders {
			for si, sym := range syms {
				bid, ask, bidSz, askSz := priceFor(s.lp.Code, sym, si, tick)
				msg := build35W(sym, bid, ask, bidSz, askSz)
				if _, werr := pc.WriteTo(msg, nil, s.dst); werr != nil {
					logger.Warn("송신 실패", slog.String("lp", s.lp.Code), slog.Any("error", werr))
				}
			}
		}
		logger.Info("송신", slog.Int("tick", tick), slog.Int("lp", len(senders)), slog.Int("symbols", len(syms)))
		tick++
	}

	if *once {
		send()
		return
	}
	t := time.NewTicker(*interval)
	defer t.Stop()
	send()
	for {
		select {
		case <-sig:
			logger.Info("종료")
			return
		case <-t.C:
			send()
		}
	}
}

// priceFor — LP별로 결정적이되 살짝 다른 top-of-book(cross/best-of-N 관측용) + avail.
// LP 코드 해시로 오프셋을 줘 per-source 구분, tick 으로 미세 변동.
func priceFor(lpCode, sym string, symIdx, tick int) (bid, ask, bidSz, askSz float64) {
	base := map[string]float64{
		"USD/KRW": 1380.00, "EUR/USD": 1.0850, "USD/JPY": 147.50,
	}
	b, okb := base[sym]
	if !okb {
		b = 100.0 + float64(symIdx)*10
	}
	// LP 오프셋: 코드별로 distinct 하도록 위치가중 해시(31배수 FNV류) % 17 → 0..16 pip.
	// 단순 바이트 합은 NHB/SHB/JPM 처럼 서로 다른 코드가 같은 버킷에 충돌해 값이
	// 겹칠 수 있어(실 LP 는 각자 실가격), 화면별 구분이 흐려짐 — 이를 방지.
	var h int
	for i := 0; i < len(lpCode); i++ {
		h = h*31 + int(lpCode[i])
	}
	if h < 0 {
		h = -h
	}
	pip := b * 0.00005 // 0.5bp
	off := float64(h%17) * pip
	drift := float64(tick%10) * pip
	spread := b * 0.0002 // 2bp
	bid = round4(b + off + drift - spread/2)
	ask = round4(b + off + drift + spread/2)
	// avail: LP별로 다른 규모 (100만 단위) — 승자 산정 시 winning-source size 관측
	bidSz = float64((h%3+1)*1_000_000) + float64(tick%5)*100_000
	askSz = float64((h%4+1)*1_000_000) + float64(tick%5)*100_000
	return
}

func round4(f float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 4, 64), 64)
	return v
}

// build35W — FIX 4.4 MarketDataSnapshotFullRefresh(top-of-book + avail). fixmd.ParseSnapshot 호환.
func build35W(sym string, bid, ask, bidSz, askSz float64) []byte {
	p := []string{
		"8=FIX.4.4", "35=W", "55=" + sym, "268=2",
		"269=0", "270=" + strconv.FormatFloat(bid, 'f', 4, 64), "271=" + strconv.FormatFloat(bidSz, 'f', 0, 64),
		"269=1", "270=" + strconv.FormatFloat(ask, 'f', 4, 64), "271=" + strconv.FormatFloat(askSz, 'f', 0, 64),
		"10=000", "",
	}
	return []byte(strings.Join(p, "\x01"))
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
