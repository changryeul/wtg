// krx-tester — mci-edge-krx ws 수신 확인 CLI (라이브/재생 e2e 검증용).
//
//	krx-tester --url ws://127.0.0.1:8085/v1/subscribe --symbols 101V6000,105V3000
//	krx-tester --url ... --symbols 101V6000 --count 5 --json
//
// /v1/subscribe 에 붙어 {"type":"subscribe","symbols":[...]} 를 보내고, 도착하는
// fut.trade/fut.book/bond.* JSON envelope 을 덤프한다. --count 만큼 받으면 종료
// (0 이면 --timeout 까지). docs/krx-live-verify.md 의 수신 확인 단계에서 사용.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	url := flag.String("url", "ws://127.0.0.1:8085/v1/subscribe", "mci-edge-krx ws 주소")
	symbols := flag.String("symbols", "", "구독 종목코드 (콤마 구분, 비면 전종목 구독 안함)")
	count := flag.Int("count", 0, "이 개수만큼 수신하면 종료 (0=무제한, --timeout 까지)")
	timeout := flag.Duration("timeout", 30*time.Second, "전체 수신 제한시간")
	asJSON := flag.Bool("json", false, "원본 JSON 라인 그대로 출력 (미지정 시 요약)")
	flag.Parse()

	c, _, err := websocket.DefaultDialer.Dial(*url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial 실패:", err)
		os.Exit(1)
	}
	defer c.Close()
	fmt.Fprintf(os.Stderr, "연결: %s\n", *url)

	if syms := splitCSV(*symbols); len(syms) > 0 {
		sub, _ := json.Marshal(map[string]interface{}{"type": "subscribe", "symbols": syms})
		if err := c.WriteMessage(websocket.TextMessage, sub); err != nil {
			fmt.Fprintln(os.Stderr, "subscribe 실패:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "구독: %v\n", syms)
	}

	// Ctrl-C 로 정상 종료.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; _ = c.Close() }()

	deadline := time.Now().Add(*timeout)
	_ = c.SetReadDeadline(deadline)
	n := 0
	kinds := map[string]int{}
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			break
		}
		n++
		if *asJSON {
			fmt.Println(string(msg))
		} else {
			printSummary(msg, kinds)
		}
		if *count > 0 && n >= *count {
			break
		}
	}

	fmt.Fprintf(os.Stderr, "총 %d 건 수신", n)
	if len(kinds) > 0 {
		var parts []string
		for k, v := range kinds {
			parts = append(parts, fmt.Sprintf("%s=%d", k, v))
		}
		fmt.Fprintf(os.Stderr, " (%s)", strings.Join(parts, " "))
	}
	fmt.Fprintln(os.Stderr)
	if n == 0 {
		os.Exit(2) // 아무것도 못 받으면 실패 (검증 스크립트가 감지)
	}
}

// printSummary — envelope 의 kind 별 핵심 필드만 한 줄 요약.
func printSummary(msg []byte, kinds map[string]int) {
	var m map[string]interface{}
	if json.Unmarshal(msg, &m) != nil {
		fmt.Println(string(msg))
		return
	}
	kind, _ := m["kind"].(string)
	kinds[kind]++
	code, _ := m["code"].(string)
	switch kind {
	case "fut.trade", "bond.trade":
		fmt.Printf("%-11s %-13s last=%-10v diff=%-8v rate=%-7v sign=%q cdiff=%v\n",
			kind, code, m["last"], m["diff"], m["rate"], m["sign"], m["cdiff"])
	case "fut.book", "bond.book":
		fmt.Printf("%-11s %-13s askTot=%-8v bidTot=%-8v ask0=%v bid0=%v\n",
			kind, code, m["askTot"], m["bidTot"], level0(m["ask"]), level0(m["bid"]))
	case "fut.master", "bond.master":
		fmt.Printf("%-11s %-13s base=%-10v prev=%v\n", kind, code, m["basePrc"], m["prevClose"])
	case "fut.settle":
		fmt.Printf("%-11s %-13s settle=%v cd=%v\n", kind, code, m["settle"], m["settleCd"])
	default:
		fmt.Println(string(msg))
	}
}

// level0 — 호가 배열의 0단 가격/잔량 요약.
func level0(v interface{}) string {
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return "-"
	}
	l, ok := arr[0].(map[string]interface{})
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%v/%v", l["prc"], l["vol"])
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
