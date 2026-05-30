package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/winwaysystems/wtg/internal/api/middleware"
)

// emitAudit 은 ring + logger 모두에 resource 와 attrs 를 전달해야 한다.
// 5개 핸들러 (symbol/profile/.../policy) 가 공유하는 경로라 단일 진실을 검증.
func TestEmitAudit_RoundTripsResourceAndAttrs(t *testing.T) {
	ring := NewAuditRing(4)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Principal 주입 — usid 가 채워지는지 확인.
	p := &middleware.Principal{Usid: "alice"}
	ctx := middleware.ContextWithPrincipal(context.Background(), p)
	r := httptest.NewRequest("PUT", "/v1/admin/symbols/USDKRW", nil).WithContext(ctx)

	emitAudit(logger, ring, r, "symbol", "PUT_SYMBOL",
		slog.String("symbol", "USDKRW"),
		slog.Bool("active", true),
	)
	emitAudit(logger, ring, r, "policy", "POLICY_KILL_SWITCH",
		slog.Bool("active", true),
	)

	out := ring.List(0)
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	// 최신 → 오래된: policy 가 먼저, symbol 이 나중.
	if out[0].Resource != "policy" {
		t.Errorf("out[0].Resource=%q, want policy", out[0].Resource)
	}
	if out[0].Action != "POLICY_KILL_SWITCH" {
		t.Errorf("out[0].Action=%q", out[0].Action)
	}
	if out[1].Resource != "symbol" || out[1].Action != "PUT_SYMBOL" {
		t.Errorf("out[1]=%+v", out[1])
	}
	if v, _ := out[1].Attrs["symbol"].(string); v != "USDKRW" {
		t.Errorf("attrs.symbol=%v, want USDKRW", out[1].Attrs["symbol"])
	}
	if v, _ := out[1].Attrs["active"].(bool); !v {
		t.Errorf("attrs.active=%v, want true", out[1].Attrs["active"])
	}
	if out[1].Usid != "alice" {
		t.Errorf("usid=%q, want alice", out[1].Usid)
	}
}

// nil ring + nil logger 면 panic 없이 no-op.
func TestEmitAudit_NilSafe(t *testing.T) {
	r := httptest.NewRequest("PUT", "/x", nil)
	emitAudit(nil, nil, r, "symbol", "PUT_SYMBOL")
}
