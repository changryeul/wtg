// mat-sise-bridge — mci-price 시세를 매칭엔진(mat) 의 시세 SHM 으로 공급하는 브리지.
//
// 아키텍처: WTG 에서 시세는 mci-price 가 단일 출처다. 매칭엔진(mat) 의
// f_get_cust_prc 는 현재 시장가(index->sise_curr) 위에 고객 마진을 얹어 체결가를
// 산출한다. 그 "현재 시장가" 를 mci-price 에서 받아 mat 으로 전달하는 것이 이 브리지다.
//
//	mci-price ─SubscribeAlgo(raw BEST)─▶ mat-sise-bridge ─APSISE UDP mcast─▶ mat_sis ─▶ Mat SHM(sise_curr)
//	                                                                                       └▶ f_get_cust_prc (마진 적용)
//
// SubscribeAlgo 는 profile 무관 raw BEST bid/ask 를 준다(마진 미적용) — mat 이 자체
// 마진을 적용하므로 정확히 맞는 소스다. 마진은 mat 이, 시세는 mci-price 가 담당.
//
// APSISE 는 mat/lib/include/sise.h 의 바이너리 전문(128B, native little-endian).
// mat_sis 는 excode='B', type="FA", symb="USD/KRW"(base@0/cont@4) 만 수용한다.
//
// 사용:
//
//	./build/bin/mat-sise-bridge \
//	    --target 127.0.0.1:50051 \
//	    --symbols USDKRW \
//	    --mcast 224.0.0.1:30022 \
//	    --iface lo
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// szAPSISE 는 sise.h 의 APSISE 구조체 크기 (native x86-64 정렬).
const szAPSISE = 128

func main() {
	var (
		target   = flag.String("target", "127.0.0.1:50051", "mci-price gRPC host:port")
		clientID = flag.String("client-id", "mat-sise-bridge", "SubscribeAlgo clientID")
		symbols  = flag.String("symbols", "USDKRW", "구독 심볼 CSV (빈값=전체)")
		mcast    = flag.String("mcast", "224.0.0.1:30022", "mat_sis 시세 multicast host:port")
		iface    = flag.String("iface", "", "multicast 송신 인터페이스 (예: lo, eth0). 빈값=기본")
		verbose  = flag.Bool("v", false, "매 전송 로그")
	)
	flag.Parse()

	// --- multicast UDP 송신 소켓 ---
	udpAddr, err := net.ResolveUDPAddr("udp4", *mcast)
	if err != nil {
		fatal("resolve mcast:", err)
	}
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		fatal("listen udp:", err)
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	// 같은 호스트의 mat_sis 로 loopback 전달 필요.
	if err := pc.SetMulticastLoopback(true); err != nil {
		fmt.Fprintln(os.Stderr, "warn: set mcast loopback:", err)
	}
	if *iface != "" {
		ifi, err := net.InterfaceByName(*iface)
		if err != nil {
			fatal("iface:", err)
		}
		if err := pc.SetMulticastInterface(ifi); err != nil {
			fatal("set mcast iface:", err)
		}
	}

	// --- mci-price gRPC ---
	gconn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial:", err)
	}
	defer gconn.Close()
	cli := wtgpb.NewPriceServiceClient(gconn)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	req := &wtgpb.AlgoSubscribeRequest{
		ClientId: *clientID,
		Symbols:  splitCSV(*symbols),
		// Sources 빈값 = BEST 모드 (source="BEST", 마진 미적용 raw bid/ask).
	}
	stream, err := cli.SubscribeAlgo(ctx, req)
	if err != nil {
		fatal("subscribe:", err)
	}
	fmt.Printf("[mat-sise-bridge] 시작 target=%s symbols=%v mcast=%s\n",
		*target, req.GetSymbols(), *mcast)

	var sent int64
	for {
		q, err := stream.Recv()
		if err == io.EOF {
			fmt.Println("[mat-sise-bridge] stream EOF")
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintln(os.Stderr, "recv:", err)
			break
		}
		// BEST(합성) / CROSS(재정) 만 시세로 공급. per-source 는 skip.
		src := q.GetSource()
		if src != "BEST" && src != "CROSS" {
			continue
		}
		symb := toPairSymb(q.GetSym())
		if symb == "" {
			continue // 통화쌍 파싱 불가 (6자 아님)
		}
		pkt := encodeAPSISE(symb, q.GetBid(), q.GetAsk(), q.GetMid(), q.GetLast(), time.Now())
		if _, err := conn.WriteTo(pkt, udpAddr); err != nil {
			fmt.Fprintln(os.Stderr, "sendto:", err)
			continue
		}
		sent++
		if *verbose || sent <= 5 || sent%200 == 0 {
			fmt.Printf("[#%06d] %s bid=%.4f ask=%.4f mid=%.4f last=%.4f\n",
				sent, symb, q.GetBid(), q.GetAsk(), q.GetMid(), q.GetLast())
		}
	}
	fmt.Printf("[mat-sise-bridge] 종료 — sent=%d\n", sent)
}

// toPairSymb 은 WTG 심볼("USDKRW" 또는 "USD/KRW")을 APSISE symb 형식("USD/KRW",
// base@0..2 / '/'@3 / cont@4..6) 으로 변환한다. 파싱 불가면 빈 문자열.
func toPairSymb(sym string) string {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if i := strings.IndexAny(sym, "/_-"); i == 3 && len(sym) == 7 {
		return sym[:3] + "/" + sym[4:] // 이미 구분자 있음 → 정규화
	}
	if len(sym) == 6 {
		return sym[:3] + "/" + sym[3:]
	}
	return ""
}

// encodeAPSISE 는 sise.h APSISE 바이너리 전문(128B, native little-endian) 을 만든다.
// 필드 오프셋은 x86-64 기본 정렬 기준(time[9] 뒤 3B 패딩 → double 8B 정렬).
func encodeAPSISE(symb string, bid, ask, mid, last float64, now time.Time) []byte {
	b := make([]byte, szAPSISE)
	copy(b[0:2], "FA")               // type — FA(spot), FB(swap)는 mat_sis 가 거름
	b[2] = 'B'                        // excode — mat_sis 는 'B'(BEST) 만 수용
	b[3] = 'B'                        // bidex
	b[4] = 'B'                        // offerex
	copyFixed(b[5:12], symb)          // symb[7] "USD/KRW"
	copyFixed(b[12:44], symb+now.Format("20060102150405")) // id[32] (진단용)
	copyFixed(b[44:52], now.Format("20060102"))            // date[8]
	copyFixed(b[52:61], fmt.Sprintf("%s%03d", now.Format("150405"), now.Nanosecond()/1e6)) // time[9] HHMMSSSSS
	// @61..63 padding (0)
	putF64(b[64:72], bid)  // usdbid  (USD/KRW 기준가; 본 심볼이 USD/KRW 면 bid 와 동일)
	putF64(b[72:80], ask)  // usdoffer
	putF64(b[80:88], bid)  // bidprc
	putF64(b[88:96], ask)  // offerprc
	putF64(b[96:104], 0)   // bidqty  (USD/KRW 는 항상 0)
	putF64(b[104:112], 0)  // offerqty
	putF64(b[112:120], mid) // midprc
	putF64(b[120:128], last) // fillprc (최근 시장 체결가)
	return b
}

func putF64(dst []byte, v float64) { binary.LittleEndian.PutUint64(dst, math.Float64bits(v)) }

// copyFixed 는 고정폭 바이트 필드에 문자열을 복사(초과분 절단, 나머지는 0).
func copyFixed(dst []byte, s string) {
	for i := range dst {
		if i < len(s) {
			dst[i] = s[i]
		} else {
			dst[i] = 0
		}
	}
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatal(args ...any) {
	fmt.Fprintln(os.Stderr, append([]any{"[mat-sise-bridge]"}, args...)...)
	os.Exit(1)
}
