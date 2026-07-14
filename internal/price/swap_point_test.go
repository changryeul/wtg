package price

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// memDocStore — 테스트용 in-memory DocStore.
type memDocStore struct {
	doc  []byte
	rev  int64
	fail int // CAS 실패 유도 횟수
}

func (m *memDocStore) Load() ([]byte, int64, error) { return m.doc, m.rev, nil }
func (m *memDocStore) CAS(next []byte, rev int64) (bool, error) {
	if m.fail > 0 {
		m.fail--
		m.rev++ // 경합 시뮬레이션
		return false, nil
	}
	if rev != m.rev {
		return false, nil
	}
	m.doc, m.rev = next, m.rev+1
	return true, nil
}

func postSwap(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/pricing/swap", strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func TestSwapPointHandler(t *testing.T) {
	st := &memDocStore{doc: []byte(`{"version":1,"swap_point":[]}`), rev: 7}
	h := SwapPointHandler(SwapPointDeps{Store: st}, false)

	w := postSwap(t, h, `{"pair":"USD/KRW","updates":[{"tenor":"1M","bid":0.15,"ask":0.25}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(st.doc), `"tenor":"1M"`) || !strings.Contains(string(st.doc), `"version":2`) {
		t.Fatalf("doc 미반영: %s", st.doc)
	}

	// clear — pair 단위 삭제
	w = postSwap(t, h, `{"pair":"USD/KRW","clear":true}`)
	if w.Code != http.StatusOK || strings.Contains(string(st.doc), "1M") {
		t.Fatalf("clear 실패: code=%d doc=%s", w.Code, st.doc)
	}
}

func TestSwapPointHandler_Validation(t *testing.T) {
	h := SwapPointHandler(SwapPointDeps{Store: &memDocStore{}}, false)
	for _, body := range []string{``, `{"updates":[{"tenor":"1M"}]}`, `not-json`} {
		if w := postSwap(t, h, body); w.Code != http.StatusBadRequest {
			t.Fatalf("body=%q → code=%d, want 400", body, w.Code)
		}
	}
}

func TestSwapPointHandler_CASRetry(t *testing.T) {
	st := &memDocStore{doc: []byte(`{"version":1}`), rev: 1, fail: 2} // 2회 경합 후 성공
	h := SwapPointHandler(SwapPointDeps{Store: st}, false)
	w := postSwap(t, h, `{"pair":"USD/KRW","updates":[{"tenor":"1M","bid":1,"ask":2}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("CAS 재시도 실패: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSwapPointHandler_NoStore(t *testing.T) {
	h := SwapPointHandler(SwapPointDeps{}, false)
	if w := postSwap(t, h, `{"pair":"X","clear":true}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("etcd 미구성은 503: %d", w.Code)
	}
}
