package fix

import (
	"errors"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/executionreport"
	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/tag"
	"github.com/shopspring/decimal"
)

// exec_report.go — ExecutionReport (35=8) 송신.
//
// Phase B-2 drop copy 경로. 매매 엔진이 발생시킨 ExecutionReport 를 mci-edge-fix
// 가 HTTP receive (POST /v1/internal/exec-report) 로 받아 해당 FIX session 으로
// quickfix.SendToTarget.

// ExecReportPayload — 매매 엔진 → mci-edge-fix POST /v1/internal/exec-report
// 의 JSON body.
//
// 필수: TargetSenderCompID + OrderID + ExecID + ExecType + OrdStatus + Side
// (LeavesQty / CumQty / AvgPx 가 0 면 0 으로 송신 — New 등 초기 단계 정상).
type ExecReportPayload struct {
	// TargetSenderCompID — 어느 FIX session 으로 보낼지. counterparty 의
	// SenderCompID 와 일치. quickfix 의 SessionID 의 TargetCompID (mci-edge-fix
	// 입장에선 외부 client) 와 같다.
	TargetSenderCompID string `json:"target_sender_comp_id"`

	OrderID       string `json:"order_id"`        // FIX tag 37 — 엔진 채번 ID
	ClientOrderID string `json:"client_order_id"` // FIX tag 11 — client 의 원래 ClOrdID
	ExecID        string `json:"exec_id"`         // FIX tag 17 — execution 채번
	ExecType      string `json:"exec_type"`       // FIX tag 150 — "0"=New / "1"=Partial / "2"=Filled / "4"=Canceled / "8"=Rejected
	OrdStatus     string `json:"ord_status"`      // FIX tag 39 — 동일 코드체계
	Side          string `json:"side"`            // "buy" / "sell"
	Symbol        string `json:"symbol"`          // FIX tag 55

	LeavesQty float64 `json:"leaves_qty"` // FIX tag 151 — 남은 수량
	CumQty    float64 `json:"cum_qty"`    // FIX tag 14 — 누적 체결 수량
	AvgPx     float64 `json:"avg_px"`     // FIX tag 6 — 평균 체결가

	LastPx  float64 `json:"last_px,omitempty"`  // FIX tag 31 — 직전 체결가 (Trade 시)
	LastQty float64 `json:"last_qty,omitempty"` // FIX tag 32

	// OrdRejReason — 거부 사유의 mymq errn (예: "1029"). orderrej_reason.go 의
	// mapOrdRejReason 가 FIX tag 103 으로 변환.
	OrdRejReason string `json:"ord_rej_reason,omitempty"`
	Text         string `json:"text,omitempty"` // FIX tag 58 — 사람 읽기용 부가 정보
}

// Validate — 필수 필드 검증.
func (p *ExecReportPayload) Validate() error {
	if p.TargetSenderCompID == "" {
		return errors.New("target_sender_comp_id 필요")
	}
	if p.OrderID == "" {
		return errors.New("order_id 필요")
	}
	if p.ExecID == "" {
		return errors.New("exec_id 필요")
	}
	if p.ExecType == "" {
		return errors.New("exec_type 필요")
	}
	if p.OrdStatus == "" {
		return errors.New("ord_status 필요")
	}
	if p.Side == "" {
		return errors.New("side 필요")
	}
	return nil
}

// buildExecutionReport — payload → quickfix.Message (35=8).
//
// FIX 4.4 의 ExecutionReport required fields: OrderID(37) / ExecID(17) /
// ExecType(150) / OrdStatus(39) / Side(54) / LeavesQty(151) / CumQty(14) /
// AvgPx(6). 그 외는 선택.
func buildExecutionReport(p ExecReportPayload) (*quickfix.Message, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	fixSide, err := sideToFIX(p.Side)
	if err != nil {
		return nil, err
	}
	er := executionreport.New(
		field.NewOrderID(p.OrderID),
		field.NewExecID(p.ExecID),
		field.NewExecType(enum.ExecType(p.ExecType)),
		field.NewOrdStatus(enum.OrdStatus(p.OrdStatus)),
		field.NewSide(fixSide),
		field.NewLeavesQty(decimal.NewFromFloat(p.LeavesQty), 0),
		field.NewCumQty(decimal.NewFromFloat(p.CumQty), 0),
		field.NewAvgPx(decimal.NewFromFloat(p.AvgPx), 5),
	)
	if p.Symbol != "" {
		er.SetSymbol(p.Symbol)
	}
	if p.ClientOrderID != "" {
		er.SetClOrdID(p.ClientOrderID)
	}
	if p.LastPx > 0 {
		er.SetLastPx(decimal.NewFromFloat(p.LastPx), 5)
	}
	if p.LastQty > 0 {
		er.SetLastQty(decimal.NewFromFloat(p.LastQty), 0)
	}
	if p.OrdRejReason != "" {
		// orderrej_reason.go 가 mymq errn → FIX tag 103 값으로 매핑.
		reason, text := mapOrdRejReason(p.OrdRejReason)
		if reason != "" {
			// OrdRejReason 은 int field — string 으로 SetField 사용.
			msg := er.ToMessage()
			msg.Body.SetString(tag.OrdRejReason, reason)
			if text != "" && p.Text == "" {
				msg.Body.SetString(tag.Text, text)
			}
			if p.Text != "" {
				msg.Body.SetString(tag.Text, p.Text)
			}
			return msg, nil
		}
	}
	if p.Text != "" {
		er.SetText(p.Text)
	}
	return er.ToMessage(), nil
}

// sideToFIX — "buy"/"sell" → FIX Side enum.
func sideToFIX(s string) (enum.Side, error) {
	switch s {
	case "buy", "BUY", "B":
		return enum.Side_BUY, nil
	case "sell", "SELL", "S":
		return enum.Side_SELL, nil
	default:
		return "", errors.New("side 미지원: " + s)
	}
}

// SendExecReport — payload 의 TargetSenderCompID 의 active session 으로
// ExecutionReport 송신.
//
// Server 메서드 — fixApp 의 active session map 에서 lookup. 등록 안 된 (또는
// Logon 통과 안 된) target 은 에러.
func (s *Server) SendExecReport(p ExecReportPayload) error {
	if err := p.Validate(); err != nil {
		return err
	}
	// SessionID 구성 — mci-edge-fix 가 acceptor 이므로 SenderCompID 가 self
	// (cfg.SenderCompID), TargetCompID 가 외부 client (payload.TargetSenderCompID).
	sid := quickfix.SessionID{
		BeginString:  "FIX.4.4",
		SenderCompID: s.cfg.SenderCompID,
		TargetCompID: p.TargetSenderCompID,
	}
	// active session 확인 — Logon 통과한 카운터파티만.
	s.app.mu.RLock()
	_, ok := s.app.active[sid.String()]
	s.app.mu.RUnlock()
	if !ok {
		s.app.execReportRejected.Add(1)
		getMetrics().execRejected.Inc()
		return errors.New("target session 미활성: " + p.TargetSenderCompID)
	}
	msg, err := buildExecutionReport(p)
	if err != nil {
		s.app.execReportRejected.Add(1)
		getMetrics().execRejected.Inc()
		return err
	}
	if err := quickfix.SendToTarget(msg, sid); err != nil {
		s.app.execReportRejected.Add(1)
		getMetrics().execRejected.Inc()
		return err
	}
	s.app.execReportSent.Add(1)
	getMetrics().execSent.Inc()
	return nil
}
