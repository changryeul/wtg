// krx-verify — KRX 원 TR 파서 런타임 대조 도구.
//
//	krx-verify gen    <capture.dat>   # 결정적 원 TR 레코드(length-prefixed) 생성
//	krx-verify decode <capture.dat>   # 같은 파일을 pkg/krx 로 디코드 → 정규 CSV
//
// C 오라클(cside/krxverify/oracle.c, 실제 sise 구조체 캐스팅)이 같은 파일을 읽은
// CSV 와 diff 하면, WTG Go 디코더의 오프셋/파싱을 C 구조체 레이아웃 기준으로
// 런타임 대조하게 된다 (scripts/krx-verify.sh). docs/krx-sise-design.md §11.7.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	wire "github.com/winwaysystems/wtg/pkg/krx"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage:")
		fmt.Fprintln(os.Stderr, "  krx-verify gen      <capture.dat>")
		fmt.Fprintln(os.Stderr, "  krx-verify decode   <capture.dat>")
		fmt.Fprintln(os.Stderr, "  krx-verify replay   <capture.dat> <group:port> [count] [interval]")
		fmt.Fprintln(os.Stderr, "  krx-verify scenario <scenario.json> <out.dat>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "gen":
		if err := gen(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "gen:", err)
			os.Exit(1)
		}
	case "decode":
		if err := decode(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "decode:", err)
			os.Exit(1)
		}
	case "replay":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: krx-verify replay <capture.dat> <group:port> [count] [interval]")
			os.Exit(2)
		}
		if err := replay(os.Args[2], os.Args[3], os.Args[4:]); err != nil {
			fmt.Fprintln(os.Stderr, "replay:", err)
			os.Exit(1)
		}
	case "scenario":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: krx-verify scenario <scenario.json> <out.dat>")
			os.Exit(2)
		}
		if err := scenarioGen(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, "scenario:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", os.Args[1])
		os.Exit(2)
	}
}

// ── gen ────────────────────────────────────────────────────────────────────

// put 는 [off,off+n) 을 공백으로 채운 뒤 s(좌측정렬) 복사 — 고정폭 필드.
func put(b []byte, off, n int, s string) {
	for i := 0; i < n; i++ {
		b[off+i] = ' '
	}
	if len(s) > n {
		s = s[:n]
	}
	copy(b[off:off+n], s)
}

func gen(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	recs := [][]byte{buildA306F(), buildA301K(), buildB606F(), buildB601K(),
		buildH306F(), buildA006F(), buildA001B()}
	for _, r := range recs {
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(r)))
		if _, err := w.Write(lp[:]); err != nil {
			return err
		}
		if _, err := w.Write(r); err != nil {
			return err
		}
	}
	return nil
}

func buildA306F() []byte {
	b := make([]byte, wire.SZA306F)
	put(b, 0, len(b), "")
	put(b, 0, 5, "A306F")
	put(b, 17, 12, "101V6000")
	put(b, 35, 12, "090005123456")
	put(b, 47, 9, "265.75")  // cprc
	put(b, 56, 9, "3")       // cvol
	put(b, 65, 9, "265.75")  // nprc
	put(b, 74, 9, "266.10")  // fprc
	put(b, 83, 9, "265.50")  // oprc
	put(b, 92, 9, "265.80")  // hprc
	put(b, 101, 9, "265.00") // lprc
	put(b, 110, 9, "265.70") // pprc
	put(b, 119, 12, "12345") // tvol
	put(b, 131, 22, "3271500000.00")
	put(b, 153, 1, "2")      // ftcd
	put(b, 154, 9, "291.00") // uldp
	put(b, 163, 9, "240.00") // lldp
	return b
}

func buildA301K() []byte {
	b := make([]byte, wire.SZA301K)
	put(b, 0, len(b), "")
	put(b, 0, 5, "A301K")
	put(b, 17, 12, "KR1035020310")
	put(b, 29, 12, "090005123456")
	put(b, 41, 11, "10250.50") // cprc
	put(b, 52, 10, "500")      // cvol
	put(b, 70, 22, "5125250.000")
	put(b, 92, 13, "3.125000")  // tyld
	put(b, 105, 11, "10240.00") // oprc
	put(b, 116, 11, "10260.00") // hprc
	put(b, 127, 11, "10235.00") // lprc
	put(b, 138, 13, "3.140000") // oyld
	put(b, 151, 13, "3.110000") // hyld
	put(b, 164, 13, "3.150000") // lyld
	put(b, 177, 15, "123456")   // tvol
	put(b, 192, 22, "1200000000.000")
	return b
}

func buildB606F() []byte {
	b := make([]byte, wire.SZB606F)
	put(b, 0, len(b), "")
	put(b, 0, 5, "B606F")
	put(b, 17, 12, "101V6000")
	put(b, 35, 12, "090005123456")
	for i := 0; i < 5; i++ {
		o := 47 + i*46
		put(b, o+0, 9, fmt.Sprintf("%.2f", 265.80+float64(i)))
		put(b, o+9, 9, fmt.Sprintf("%.2f", 265.70-float64(i)))
		put(b, o+18, 9, fmt.Sprintf("%d", 10+i))
		put(b, o+27, 9, fmt.Sprintf("%d", 20+i))
		put(b, o+36, 5, fmt.Sprintf("%d", 1+i))
		put(b, o+41, 5, fmt.Sprintf("%d", 2+i))
	}
	put(b, 277, 9, "1000") // stvl
	put(b, 286, 9, "1100") // btvl
	put(b, 295, 5, "10")   // apvc
	put(b, 300, 5, "11")   // bpvc
	put(b, 305, 9, "265.75")
	put(b, 314, 9, "50")
	return b
}

func buildB601K() []byte {
	b := make([]byte, wire.SZB601K)
	put(b, 0, len(b), "")
	put(b, 0, 5, "B601K")
	put(b, 17, 12, "KR1035020310")
	put(b, 29, 12, "090005123456")
	for i := 0; i < 5; i++ {
		o := 41 + i*78
		put(b, o+0, 11, fmt.Sprintf("%.2f", 10251.0+float64(i)))
		put(b, o+11, 11, fmt.Sprintf("%.2f", 10250.0-float64(i)))
		put(b, o+22, 15, fmt.Sprintf("%d", 100+i))
		put(b, o+37, 15, fmt.Sprintf("%d", 200+i))
		put(b, o+52, 13, fmt.Sprintf("%.6f", 3.10+0.01*float64(i)))
		put(b, o+65, 13, fmt.Sprintf("%.6f", 3.20+0.01*float64(i)))
	}
	put(b, 431, 15, "5000") // stvl
	put(b, 446, 15, "6000") // btvl
	return b
}

func buildH306F() []byte {
	b := make([]byte, wire.SZH306F)
	put(b, 0, len(b), "")
	put(b, 0, 5, "H306F")
	put(b, 5, 12, "101V6000")
	put(b, 23, 18, "265.30") // sprc
	put(b, 41, 2, "11")      // spcd
	put(b, 43, 8, "265.40")  // lspr
	put(b, 51, 1, "1")       // lspc
	return b
}

func buildA006F() []byte {
	b := make([]byte, wire.SZMaster)
	put(b, 0, len(b), "")
	put(b, 0, 5, "A006F")
	put(b, 27, 12, "201S3000")
	put(b, 45, 1, "C")             // focd
	put(b, 331, 11, "15.20")       // upl1
	put(b, 364, 11, "2.10")        // lpl1
	put(b, 397, 11, "8.55")        // bprc
	put(b, 411, 1, "E")            // recd
	put(b, 465, 18, "425.00")      // eprc
	put(b, 484, 22, "1.00")        // unit
	put(b, 506, 22, "250000.00")   // mult
	put(b, 543, 12, "K2I00000000") // uacd
	put(b, 689, 1, "0")            // halt
	put(b, 730, 1, "1")            // atmc
	put(b, 748, 11, "8.60")        // yprc
	put(b, 825, 12, "45231")       // pdoi
	put(b, 859, 11, "0.1875")      // ipvl
	return b
}

func buildA001B() []byte {
	b := make([]byte, wire.SZBondMaster)
	put(b, 0, len(b), "")
	put(b, 0, 5, "A001B")
	put(b, 27, 12, "KR1035020310")
	put(b, 172, 13, "1.500000") // isrt
	put(b, 185, 14, "3.250000") // cprt
	put(b, 294, 11, "10240.00") // bprc
	return b
}

// ── scenario ─────────────────────────────────────────────────────────────

// scenSym — FEP 시나리오 한 종목 (마스터+체결+호가[+정산]). 값은 FEP 가상 시세에
// 맞춰 JSON 에서 지정. market: "fut"(파생 A006F/A306F/B606F/H306F) | "bond"(채권
// A001B/A301K/B601K). book 레벨은 [가격,잔량,건수(fut)|수익률(bond)].
type scenSym struct {
	Market string `json:"market"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Master struct {
		Yprc, Bprc, Strike, UpLimit, DnLimit float64
		OptType                              string
	} `json:"master"`
	Trade struct {
		Cprc, Pprc, Oprc, Hprc, Lprc, Tyld float64
		Cvol, Tvol                         int64
		Bs                                 string
	} `json:"trade"`
	Book struct {
		Ask [][]float64 `json:"ask"`
		Bid [][]float64 `json:"bid"`
	} `json:"book"`
	Settle *struct {
		Sprc, Lspr float64
		Spcd       string
	} `json:"settle"`
}

// scenarioGen 은 시나리오 JSON → length-prefixed 캡처. 종목별로 마스터→정산→호가→체결
// 순서로 emit (체결 전에 마스터/정산이 캐시돼야 전일대비/정산가가 채워짐).
func scenarioGen(jsonPath, outPath string) error {
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}
	var syms []scenSym
	if err := json.Unmarshal(raw, &syms); err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	emit := func(rec []byte) error {
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(rec)))
		if _, err := w.Write(lp[:]); err != nil {
			return err
		}
		_, err := w.Write(rec)
		return err
	}
	for _, s := range syms {
		var recs [][]byte
		if s.Market == "bond" {
			recs = [][]byte{scenA001B(s), scenB601K(s), scenA301K(s)}
		} else {
			recs = [][]byte{scenA006F(s)}
			if s.Settle != nil {
				recs = append(recs, scenH306F(s))
			}
			recs = append(recs, scenB606F(s), scenA306F(s))
		}
		for _, r := range recs {
			if err := emit(r); err != nil {
				return err
			}
		}
	}
	fmt.Fprintf(os.Stderr, "scenario %d 종목 → %s\n", len(syms), outPath)
	return nil
}

// blank — n바이트 공백 프레임.
func blank(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return b
}

func scenA006F(s scenSym) []byte {
	b := blank(wire.SZMaster)
	put(b, 0, 5, "A006F")
	put(b, 27, 12, s.Code)
	opt := s.Master.OptType
	if opt == "" {
		opt = "F"
	}
	put(b, 45, 1, opt)
	put(b, 331, 11, fmt.Sprintf("%.2f", s.Master.UpLimit))
	put(b, 364, 11, fmt.Sprintf("%.2f", s.Master.DnLimit))
	put(b, 397, 11, fmt.Sprintf("%.2f", s.Master.Bprc))
	put(b, 411, 1, "E")
	put(b, 465, 18, fmt.Sprintf("%.2f", s.Master.Strike))
	put(b, 748, 11, fmt.Sprintf("%.2f", s.Master.Yprc))
	return b
}

func scenH306F(s scenSym) []byte {
	b := blank(wire.SZH306F)
	put(b, 0, 5, "H306F")
	put(b, 5, 12, s.Code)
	put(b, 23, 18, fmt.Sprintf("%.2f", s.Settle.Sprc))
	put(b, 41, 2, s.Settle.Spcd)
	put(b, 43, 8, fmt.Sprintf("%.2f", s.Settle.Lspr))
	put(b, 51, 1, "1")
	return b
}

func scenA306F(s scenSym) []byte {
	b := blank(wire.SZA306F)
	put(b, 0, 5, "A306F")
	put(b, 17, 12, s.Code)
	put(b, 35, 12, "090005123456")
	put(b, 47, 9, fmt.Sprintf("%.2f", s.Trade.Cprc))
	put(b, 56, 9, fmt.Sprintf("%d", s.Trade.Cvol))
	put(b, 83, 9, fmt.Sprintf("%.2f", s.Trade.Oprc))
	put(b, 92, 9, fmt.Sprintf("%.2f", s.Trade.Hprc))
	put(b, 101, 9, fmt.Sprintf("%.2f", s.Trade.Lprc))
	put(b, 110, 9, fmt.Sprintf("%.2f", s.Trade.Pprc))
	put(b, 119, 12, fmt.Sprintf("%d", s.Trade.Tvol))
	put(b, 153, 1, s.Trade.Bs)
	put(b, 154, 9, fmt.Sprintf("%.2f", s.Master.UpLimit))
	put(b, 163, 9, fmt.Sprintf("%.2f", s.Master.DnLimit))
	return b
}

func scenB606F(s scenSym) []byte {
	b := blank(wire.SZB606F)
	put(b, 0, 5, "B606F")
	put(b, 17, 12, s.Code)
	put(b, 35, 12, "090005123456")
	var st, bt int64
	for i := 0; i < 5; i++ {
		o := 47 + i*46
		if i < len(s.Book.Ask) {
			a := s.Book.Ask[i]
			put(b, o+0, 9, fmt.Sprintf("%.2f", a[0]))
			put(b, o+18, 9, fmt.Sprintf("%d", int64(a[1])))
			put(b, o+36, 5, fmt.Sprintf("%d", int64(a[2])))
			st += int64(a[1])
		}
		if i < len(s.Book.Bid) {
			d := s.Book.Bid[i]
			put(b, o+9, 9, fmt.Sprintf("%.2f", d[0]))
			put(b, o+27, 9, fmt.Sprintf("%d", int64(d[1])))
			put(b, o+41, 5, fmt.Sprintf("%d", int64(d[2])))
			bt += int64(d[1])
		}
	}
	put(b, 277, 9, fmt.Sprintf("%d", st))
	put(b, 286, 9, fmt.Sprintf("%d", bt))
	return b
}

func scenA001B(s scenSym) []byte {
	b := blank(wire.SZBondMaster)
	put(b, 0, 5, "A001B")
	put(b, 27, 12, s.Code)
	put(b, 294, 11, fmt.Sprintf("%.2f", s.Master.Bprc))
	return b
}

func scenA301K(s scenSym) []byte {
	b := blank(wire.SZA301K)
	put(b, 0, 5, "A301K")
	put(b, 17, 12, s.Code)
	put(b, 29, 12, "090005123456")
	put(b, 41, 11, fmt.Sprintf("%.2f", s.Trade.Cprc))
	put(b, 52, 10, fmt.Sprintf("%d", s.Trade.Cvol))
	put(b, 92, 13, fmt.Sprintf("%.6f", s.Trade.Tyld))
	put(b, 105, 11, fmt.Sprintf("%.2f", s.Trade.Oprc))
	put(b, 116, 11, fmt.Sprintf("%.2f", s.Trade.Hprc))
	put(b, 127, 11, fmt.Sprintf("%.2f", s.Trade.Lprc))
	put(b, 177, 15, fmt.Sprintf("%d", s.Trade.Tvol))
	return b
}

func scenB601K(s scenSym) []byte {
	b := blank(wire.SZB601K)
	put(b, 0, 5, "B601K")
	put(b, 17, 12, s.Code)
	put(b, 29, 12, "090005123456")
	var st, bt int64
	for i := 0; i < 5; i++ {
		o := 41 + i*78
		if i < len(s.Book.Ask) {
			a := s.Book.Ask[i]
			put(b, o+0, 11, fmt.Sprintf("%.2f", a[0]))
			put(b, o+22, 15, fmt.Sprintf("%d", int64(a[1])))
			put(b, o+52, 13, fmt.Sprintf("%.6f", a[2]))
			st += int64(a[1])
		}
		if i < len(s.Book.Bid) {
			d := s.Book.Bid[i]
			put(b, o+11, 11, fmt.Sprintf("%.2f", d[0]))
			put(b, o+37, 15, fmt.Sprintf("%d", int64(d[1])))
			put(b, o+65, 13, fmt.Sprintf("%.6f", d[2]))
			bt += int64(d[1])
		}
	}
	put(b, 431, 15, fmt.Sprintf("%d", st))
	put(b, 446, 15, fmt.Sprintf("%d", bt))
	return b
}

// ── replay ───────────────────────────────────────────────────────────────

// replay 는 length-prefixed 캡처의 각 레코드를 원 TR 1건 = UDP datagram 1개로
// 멀티캐스트 addr(group:port)에 송신한다 — 장외 시간에도 mci-edge-krx --mcast
// e2e 를 돌릴 수 있게 KRX 실시간(datagram 당 1 TR)을 흉내낸다.
// args: [count [interval]] — count 회 반복(기본 1), interval 간격(기본 100ms).
func replay(path, addr string, args []string) error {
	count, interval := 1, 100*time.Millisecond
	if len(args) >= 1 {
		if v, err := strconv.Atoi(args[0]); err == nil {
			count = v
		}
	}
	if len(args) >= 2 {
		if d, err := time.ParseDuration(args[1]); err == nil {
			interval = d
		}
	}
	raddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	recs, err := readRecords(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "replay %d 레코드 → %s (count=%d interval=%s)\n", len(recs), addr, count, interval)
	sent := 0
	for c := 0; c < count; c++ {
		for _, r := range recs {
			if _, err := conn.Write(r); err != nil {
				return err
			}
			sent++
			time.Sleep(interval)
		}
	}
	fmt.Fprintf(os.Stderr, "송신 %d datagram\n", sent)
	return nil
}

// readRecords 는 length-prefixed 캡처를 payload 슬라이스 목록으로 읽는다.
func readRecords(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var out [][]byte
	var lp [4]byte
	for {
		if _, err := io.ReadFull(r, lp[:]); err != nil {
			break
		}
		n := binary.BigEndian.Uint32(lp[:])
		if n == 0 || n > 70000 {
			break
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		out = append(out, buf)
	}
	return out, nil
}

// ── decode ───────────────────────────────────────────────────────────────

func decode(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	var lp [4]byte
	for {
		if _, err := io.ReadFull(r, lp[:]); err != nil {
			break
		}
		n := binary.BigEndian.Uint32(lp[:])
		if n == 0 || n > 70000 {
			break
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		if n < 5 {
			continue
		}
		line, err := decodeOne(buf)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

func decodeOne(b []byte) (string, error) {
	switch string(b[0:5]) {
	case "A306F":
		t, err := wire.DecodeA306F(b)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("A306F,%s,last=%.4f,open=%.4f,high=%.4f,low=%.4f,near=%.4f,far=%.4f,pprc=%.4f,"+
			"cvol=%d,tvol=%d,tamt=%.4f,uplim=%.4f,dnlim=%.4f,bs=%s",
			t.Code, t.Last, t.Open, t.High, t.Low, t.NearPrc, t.FarPrc, t.PrevTradePrc,
			t.Cvol, t.Tvol, t.Tamt, t.UpLimit, t.DnLimit, t.Bs), nil
	case "A301K":
		t, err := wire.DecodeA301K(b)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("A301K,%s,last=%.4f,yld=%.6f,cvol=%d,camt=%.4f,open=%.4f,high=%.4f,low=%.4f,"+
			"oyld=%.6f,hyld=%.6f,lyld=%.6f,tvol=%d,tamt=%.4f",
			t.Code, t.Last, t.Yield, t.Cvol, t.Camt, t.Open, t.High, t.Low,
			t.OYield, t.HYield, t.LYield, t.Tvol, t.Tamt), nil
	case "B606F":
		t, err := wire.DecodeB606F(b)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "B606F,%s,askTot=%d,bidTot=%d,askCnt=%d,bidCnt=%d,exp=%.4f,expVol=%d",
			t.Code, t.AskTot, t.BidTot, t.AskCnt, t.BidCnt, t.ExpPrc, t.ExpVol)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(&sb, ",ask%d=%.4f/%d/%d", i, t.Ask[i].Prc, t.Ask[i].Vol, t.Ask[i].Cnt)
		}
		for i := 0; i < 5; i++ {
			fmt.Fprintf(&sb, ",bid%d=%.4f/%d/%d", i, t.Bid[i].Prc, t.Bid[i].Vol, t.Bid[i].Cnt)
		}
		return sb.String(), nil
	case "B601K":
		t, err := wire.DecodeB601K(b)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "B601K,%s,askTot=%d,bidTot=%d", t.Code, t.AskTot, t.BidTot)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(&sb, ",ask%d=%.4f/%d/%.6f", i, t.Ask[i].Prc, t.Ask[i].Vol, t.Ask[i].Yld)
		}
		for i := 0; i < 5; i++ {
			fmt.Fprintf(&sb, ",bid%d=%.4f/%d/%.6f", i, t.Bid[i].Prc, t.Bid[i].Vol, t.Bid[i].Yld)
		}
		return sb.String(), nil
	case "H306F":
		t, err := wire.DecodeH306F(b)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("H306F,%s,settle=%.4f,settleCd=%s,final=%.4f,finalCd=%s",
			t.Code, t.Settle, t.SettleCd, t.FinalSettle, t.FinalSettleCd), nil
	case "A006F":
		t, err := wire.DecodeA006F(b)
		if err != nil {
			return "", err
		}
		halt := "0"
		if t.Halt {
			halt = "1"
		}
		return fmt.Sprintf("A006F,%s,base=%.4f,prev=%.4f,uplim=%.4f,dnlim=%.4f,strike=%.4f,mult=%.4f,unit=%.4f,"+
			"prevOI=%d,iv=%.4f,focd=%s,recd=%s,atmc=%s,halt=%s,uacd=%s",
			t.Code, t.BasePrc, t.PrevClose, t.UpLimit, t.DnLimit, t.Strike, t.Mult, t.Unit,
			t.PrevOI, t.IV, t.OptType, t.ExerciseType, t.AtmType, halt, t.UnderlyingCd), nil
	case "A001B":
		t, err := wire.DecodeA001B(b)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("A001B,%s,base=%.4f,coupon=%.6f,issueRate=%.6f",
			t.Code, t.BasePrc, t.CouponRate, t.IssueRate), nil
	default:
		return "", fmt.Errorf("미지원 TR: %q", string(b[0:5]))
	}
}
