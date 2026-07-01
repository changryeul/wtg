// fix-md-tester — mci-edge-md smoke test 도구.
//
// quickfix initiator 로 Logon → MarketDataRequest (35=V) 1건 송신 → 스냅샷
// (35=W) 수신 출력 후 종료. 심볼별 bid/offer 를 표로 출력.
//
// 사용:
//
//	./build/bin/fix-md-tester \
//	    --target 127.0.0.1:5011 \
//	    --sender ECN_MD_TEST_01 \
//	    --target-comp WTG_MD \
//	    --password test-pw \
//	    --symbols USD/KRW,EUR/USD
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/marketdatarequest"
	"github.com/quickfixgo/quickfix"
)

type app struct {
	logonDone   chan struct{}
	password    string
	recvSnaps   atomic.Int32
	logoutCount atomic.Int32
}

func (a *app) OnCreate(sid quickfix.SessionID) {
	fmt.Printf("[fix-md-tester] session created sender=%s target=%s\n",
		sid.SenderCompID, sid.TargetCompID)
}
func (a *app) OnLogon(sid quickfix.SessionID) {
	fmt.Println("[fix-md-tester] LOGON OK")
	select {
	case a.logonDone <- struct{}{}:
	default:
	}
}
func (a *app) OnLogout(sid quickfix.SessionID) {
	a.logoutCount.Add(1)
	fmt.Println("[fix-md-tester] LOGOUT")
}
func (a *app) ToAdmin(msg *quickfix.Message, sid quickfix.SessionID) {
	if t, err := msg.MsgType(); err == nil && t == "A" && a.password != "" {
		msg.Body.SetString(554, a.password)
	}
}
func (a *app) FromAdmin(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}
func (a *app) ToApp(msg *quickfix.Message, sid quickfix.SessionID) error { return nil }
func (a *app) FromApp(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	mt, _ := msg.MsgType()
	switch mt {
	case "W":
		a.recvSnaps.Add(1)
		symbol, _ := msg.Body.GetString(55)
		mdReqID, _ := msg.Body.GetString(262)
		// NoMDEntries — bid/offer 추출.
		// 간단히: GetGroup 대신 tag scan 으로 훑음.
		fmt.Printf("[fix-md-tester] SNAPSHOT symbol=%s mdreq_id=%s\n", symbol, mdReqID)
		dumpEntries(msg)
	case "Y":
		reqID, _ := msg.Body.GetString(262)
		reason, _ := msg.Body.GetString(281)
		fmt.Printf("[fix-md-tester] MDR_REJECT req_id=%s reason=%s\n", reqID, reason)
	default:
		fmt.Printf("[fix-md-tester] FromApp msg_type=%s\n", mt)
	}
	return nil
}

// dumpEntries — 35=W 의 NoMDEntries 개수 + raw wire 요약 출력.
// group iteration 은 quickfix Go 에서 typed API 로 처리해야 정확하지만 Phase A
// smoke 는 심플하게 개수 + raw string 로 검증 충분.
func dumpEntries(msg *quickfix.Message) {
	nStr, _ := msg.Body.GetString(268)
	fmt.Printf("           NoMDEntries=%s\n", nStr)
	// raw wire 요약 — SOH → | 로 치환해서 human-readable.
	raw := msg.String()
	raw = strings.ReplaceAll(raw, "\x01", "|")
	fmt.Printf("           raw=%s\n", raw)
}

func main() {
	var (
		target      = flag.String("target", "127.0.0.1:5011", "mci-edge-md host:port")
		sender      = flag.String("sender", "ECN_MD_TEST_01", "SenderCompID")
		targetComp  = flag.String("target-comp", "WTG_MD", "TargetCompID (mci-edge-md self)")
		password    = flag.String("password", "", "Logon Password (tag 554). 빈값=skip")
		symbols     = flag.String("symbols", "USD/KRW", "MDR 심볼 리스트 (콤마 구분)")
		subType     = flag.String("sub-type", "0", "SubscriptionRequestType — 0=snapshot / 1=snapshot+updates / 2=unsubscribe")
		waitTime    = flag.Duration("wait", 6*time.Second, "Logon 후 스냅샷 등 수신 대기 시간")
		dialTimeout = flag.Duration("dial-timeout", 5*time.Second, "Logon 대기 timeout")
	)
	flag.Parse()

	host, port, err := splitHostPort(*target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "target 파싱:", err)
		os.Exit(2)
	}

	settings := fmt.Sprintf(`[DEFAULT]
ConnectionType=initiator
ReconnectInterval=1
SenderCompID=%s
TargetCompID=%s
SocketConnectHost=%s
SocketConnectPort=%d
BeginString=FIX.4.4
HeartBtInt=30
StartTime=00:00:00
EndTime=00:00:00
ResetOnLogon=Y
[SESSION]
TargetCompID=%s
`, *sender, *targetComp, host, port, *targetComp)

	cfg, err := quickfix.ParseSettings(strings.NewReader(settings))
	if err != nil {
		fmt.Fprintln(os.Stderr, "settings 파싱:", err)
		os.Exit(1)
	}
	a := &app{logonDone: make(chan struct{}, 1), password: *password}
	ini, err := quickfix.NewInitiator(a, quickfix.NewMemoryStoreFactory(), cfg, quickfix.NewNullLogFactory())
	if err != nil {
		fmt.Fprintln(os.Stderr, "initiator:", err)
		os.Exit(1)
	}
	if err := ini.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	defer ini.Stop()

	select {
	case <-a.logonDone:
	case <-time.After(*dialTimeout):
		fmt.Fprintln(os.Stderr, "[fix-md-tester] LOGON timeout")
		os.Exit(3)
	}

	if err := sendMDR(*sender, *targetComp, *subType, *symbols); err != nil {
		fmt.Fprintln(os.Stderr, "[fix-md-tester] MDR 송신 실패:", err)
		os.Exit(4)
	}
	fmt.Println("[fix-md-tester] MDR 송신 완료 — 스냅샷 대기...")

	time.Sleep(*waitTime)
	fmt.Printf("[fix-md-tester] 종료 — snapshots=%d logouts=%d\n",
		a.recvSnaps.Load(), a.logoutCount.Load())
}

// sendMDR — MarketDataRequest 조립 + 송신. 심볼당 tag 55 반복.
func sendMDR(sender, target, subType, symbolsCSV string) error {
	syms := strings.Split(symbolsCSV, ",")
	if len(syms) == 0 || syms[0] == "" {
		return fmt.Errorf("symbols 빈값")
	}
	reqID := fmt.Sprintf("MDR-%d", time.Now().UnixNano())

	// SubReqType enum 변환.
	var srt enum.SubscriptionRequestType
	switch subType {
	case "0":
		srt = enum.SubscriptionRequestType_SNAPSHOT
	case "1":
		srt = enum.SubscriptionRequestType_SNAPSHOT_PLUS_UPDATES
	case "2":
		srt = enum.SubscriptionRequestType_DISABLE_PREVIOUS_SNAPSHOT_PLUS_UPDATE_REQUEST
	default:
		return fmt.Errorf("sub-type 미지원: %q (0/1/2 만)", subType)
	}

	mdr := marketdatarequest.New(
		field.NewMDReqID(reqID),
		field.NewSubscriptionRequestType(srt),
		field.NewMarketDepth(0), // 0=full book
	)

	grp := marketdatarequest.NewNoRelatedSymRepeatingGroup()
	for _, s := range syms {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		row := grp.Add()
		row.SetSymbol(s)
	}
	mdr.SetNoRelatedSym(grp)

	sid := quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: sender, TargetCompID: target}
	return quickfix.SendToTarget(mdr, sid)
}

func splitHostPort(hostport string) (string, int, error) {
	i := strings.LastIndex(hostport, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("':' 없음")
	}
	host := hostport[:i]
	port, err := strconv.Atoi(hostport[i+1:])
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}
