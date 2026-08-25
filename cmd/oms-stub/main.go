// oms-stub — 주문 e2e 테스트용 OMS 응답(체결) 스텁.
//
// WTG=MCI(앞단) 경계 관점: WTG 는 주문을 broker 로 정규화·publish 하고, 체결은
// 화면에 통보한다. 실제 OMS/wfg-rs/LP 준비 전, 이 스텁이 **OMS 의 체결 응답을
// 흉내내 WTG 인바운드 push 경로로 주입**한다 → mci-push → 웹 ws 화면(그리고 옵션으로
// mci-edge-fix-ord → FIX 35=8). broker 트랜잭션 AP 응답은 pkg/mymq(클라 전용)로 못 하므로,
// 스텁은 broker 를 통하지 않고 WTG 의 체결-수신 endpoint 로 직접 밀어넣는다.
// docs/order-architecture.md (dq/D_FEX·D_Fut 이후 화면까지의 마지막 hop 시뮬레이션).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

func main() {
	// 대상 endpoint
	pushURL := flag.String("push-url", "http://127.0.0.1:8081/v1/internal/push", "mci-push HTTP push endpoint (웹 화면 체결통보)")
	pushSecret := flag.String("push-secret", "", "mci-push X-Push-Secret")
	fixURL := flag.String("fix-url", "", "mci-edge-fix-ord exec-report endpoint (예 http://127.0.0.1:5002/v1/internal/exec-report). 비면 skip")
	fixSecret := flag.String("fix-secret", "", "FIX exec-report X-Push-Secret")
	fixTarget := flag.String("fix-target", "", "FIX 대상 SenderCompID (--fix-url 사용 시 필수)")

	// 주문/체결 파라미터
	user := flag.String("user", "", "체결통보 대상 LogonID(웹). 빈값이면 broadcast")
	symbol := flag.String("symbol", "USD/KRW", "심볼")
	side := flag.String("side", "buy", "buy|sell")
	qty := flag.Float64("qty", 1000000, "주문 수량")
	px := flag.Float64("px", 1380.60, "체결가")
	lp := flag.String("lp", "SMB", "LP/소스 (SMB/KMB/EBS/CMB)")
	clOrdID := flag.String("cl-ord-id", "", "client order id (비면 자동)")
	execType := flag.String("exec", "fill", "accept|partial|fill|reject")
	rejReason := flag.String("rej-reason", "1029", "reject 시 mymq errn (FIX tag103 매핑)")
	flag.Parse()

	now := time.Now()
	stamp := now.Format("150405.000")
	if *clOrdID == "" {
		*clOrdID = "CL-" + now.Format("150405")
	}
	orderID := "OMS-" + now.Format("150405")
	execID := "EX-" + stamp

	// exec-type 프리셋 → FIX 코드(150/39) + 수량 계산.
	var fixExec, fixStatus, webExec, webStatus string
	var cum, leaves, lastQty, avgPx, lastPx float64
	switch *execType {
	case "accept":
		fixExec, fixStatus, webExec, webStatus = "0", "0", "accepted", "new"
		cum, leaves, lastQty, avgPx, lastPx = 0, *qty, 0, 0, 0
	case "partial":
		half := *qty / 2
		fixExec, fixStatus, webExec, webStatus = "1", "1", "partial", "partially_filled"
		cum, leaves, lastQty, avgPx, lastPx = half, *qty-half, half, *px, *px
	case "reject":
		fixExec, fixStatus, webExec, webStatus = "8", "8", "reject", "rejected"
		cum, leaves, lastQty, avgPx, lastPx = 0, 0, 0, 0, 0
	default: // fill
		*execType = "fill"
		fixExec, fixStatus, webExec, webStatus = "2", "2", "fill", "filled"
		cum, leaves, lastQty, avgPx, lastPx = *qty, 0, *qty, *px, *px
	}

	// ── 웹 화면용 체결 envelope (mci-push) ─────────────────────────────
	web := map[string]any{
		"type": "exec_report", "lp": *lp,
		"cl_ord_id": *clOrdID, "order_id": orderID, "exec_id": execID,
		"symbol": *symbol, "side": *side,
		"exec_type": webExec, "ord_status": webStatus,
		"order_qty": *qty, "cum_qty": cum, "leaves_qty": leaves,
		"last_qty": lastQty, "last_px": lastPx, "avg_px": avgPx,
		"ts_unix_nano": now.UnixNano(),
	}
	if *execType == "reject" {
		web["rej_reason"] = *rejReason
		web["text"] = "stub reject"
	}
	webData, _ := json.Marshal(web)
	pushBody, _ := json.Marshal(map[string]any{"user": *user, "data": json.RawMessage(webData)})

	fmt.Printf("[oms-stub] %s %s %s qty=%.0f px=%.2f lp=%s user=%q\n",
		*execType, *side, *symbol, *qty, *px, *lp, *user)

	if code, resp, err := post(*pushURL, *pushSecret, pushBody); err != nil {
		fmt.Printf("  mci-push  → 실패: %v\n", err)
	} else {
		fmt.Printf("  mci-push  → HTTP %d %s\n", code, trunc(resp))
	}

	// ── FIX exec-report (옵션, mci-edge-fix-ord → 35=8) ───────────────
	if *fixURL != "" {
		if *fixTarget == "" {
			fmt.Println("  FIX       → skip (--fix-target 필요)")
		} else {
			fixBody, _ := json.Marshal(map[string]any{
				"target_sender_comp_id": *fixTarget,
				"order_id":              orderID, "client_order_id": *clOrdID, "exec_id": execID,
				"exec_type": fixExec, "ord_status": fixStatus,
				"side": *side, "symbol": *symbol,
				"leaves_qty": leaves, "cum_qty": cum, "avg_px": avgPx,
				"last_px": lastPx, "last_qty": lastQty,
				"ord_rej_reason": func() string {
					if *execType == "reject" {
						return *rejReason
					}
					return ""
				}(),
			})
			if code, resp, err := post(*fixURL, *fixSecret, fixBody); err != nil {
				fmt.Printf("  FIX exec  → 실패: %v\n", err)
			} else {
				fmt.Printf("  FIX exec  → HTTP %d %s\n", code, trunc(resp))
			}
		}
	}
}

func post(url, secret string, body []byte) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Push-Secret", secret)
	}
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp.StatusCode, string(b), nil
}

func trunc(s string) string {
	s = string(bytes.TrimSpace([]byte(s)))
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
