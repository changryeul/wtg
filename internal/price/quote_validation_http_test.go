package price

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func mkHTTPGateway(t *testing.T) (*httptest.Server, *QuoteValidationServer) {
	t.Helper()
	srv, _ := mkValidationServer(t)
	mux := http.NewServeMux()
	RegisterQuoteValidationHTTP(mux, srv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, srv
}

// post — JSON body POST helper, response body 문자열 반환.
func post(t *testing.T, url string, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestQuoteValidationHTTP_Validate_OK(t *testing.T) {
	ts, srv := mkHTTPGateway(t)
	// 사전 — record 등록.
	now := time.Now()
	rec := mkRegRecord("A-1", now, time.Hour)
	if err := srv.registry.Put(context.Background(), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	code, body := post(t, ts.URL+"/v1/quoteid/validate",
		`{"quoteId":"A-1","engineId":"test-engine"}`)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	var resp wtgpb.ValidateResponse
	// protojson 사용 — record 안의 quoteId 포함된 JSON.
	if err := json.Unmarshal([]byte(body), &struct {
		Status string `json:"status"`
		Record struct {
			QuoteId string  `json:"quoteId"`
			Bid     float64 `json:"bid"`
			Ask     float64 `json:"ask"`
		} `json:"record"`
	}{}); err != nil {
		t.Fatalf("JSON parse: %v body=%s", err, body)
	}
	// 더 엄격한 검증 — proto-aware parse.
	_ = resp
	if !strings.Contains(body, `"status":"OK"`) {
		t.Errorf("status 누락: %s", body)
	}
	if !strings.Contains(body, `"quoteId":"A-1"`) {
		t.Errorf("quoteId echo 누락: %s", body)
	}
	if !strings.Contains(body, `"bid":1400.1`) {
		t.Errorf("bid echo 누락: %s", body)
	}
}

func TestQuoteValidationHTTP_Validate_NotFound(t *testing.T) {
	ts, _ := mkHTTPGateway(t)
	code, body := post(t, ts.URL+"/v1/quoteid/validate", `{"quoteId":"A-nope"}`)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"status":"NOT_FOUND"`) {
		t.Errorf("NOT_FOUND status 누락: %s", body)
	}
	if !strings.Contains(body, `"ordRejReason":5`) {
		t.Errorf("OrdRejReason=5 누락: %s", body)
	}
}

func TestQuoteValidationHTTP_BadJSON(t *testing.T) {
	ts, _ := mkHTTPGateway(t)
	code, _ := post(t, ts.URL+"/v1/quoteid/validate", `not-json`)
	if code != 400 {
		t.Errorf("잘못된 JSON status=%d, want 400", code)
	}
	code, _ = post(t, ts.URL+"/v1/quoteid/validate", ``)
	if code != 400 {
		t.Errorf("빈 body status=%d, want 400", code)
	}
}

func TestQuoteValidationHTTP_MethodNotAllowed(t *testing.T) {
	ts, _ := mkHTTPGateway(t)
	resp, err := http.Get(ts.URL + "/v1/quoteid/validate")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// http.ServeMux 의 "POST /xxx" 패턴은 GET 에 405 반환.
	if resp.StatusCode != 405 {
		t.Errorf("GET /validate status=%d, want 405", resp.StatusCode)
	}
}

func TestQuoteValidationHTTP_BatchValidate(t *testing.T) {
	ts, srv := mkHTTPGateway(t)
	now := time.Now()
	_ = srv.registry.Put(context.Background(), mkRegRecord("A-1", now, time.Hour))
	_ = srv.registry.Put(context.Background(), mkRegRecord("A-2", now, time.Hour))

	body := `{"quoteIds":["A-1","A-2","A-nope"],"engineId":"e"}`
	code, resp := post(t, ts.URL+"/v1/quoteid/batch-validate", body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	// results 배열 3개, 처음 둘 OK, 마지막 NOT_FOUND.
	if strings.Count(resp, `"status":"OK"`) != 2 {
		t.Errorf("OK 카운트 mismatch: %s", resp)
	}
	if strings.Count(resp, `"status":"NOT_FOUND"`) != 1 {
		t.Errorf("NOT_FOUND 카운트 mismatch: %s", resp)
	}
}

func TestQuoteValidationHTTP_BatchValidate_ExceedsMax(t *testing.T) {
	ts, _ := mkHTTPGateway(t)
	// MaxBatchValidateSize+1 quote_ids 의 JSON.
	var buf bytes.Buffer
	buf.WriteString(`{"quoteIds":[`)
	for i := 0; i < MaxBatchValidateSize+1; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`"x"`)
	}
	buf.WriteString(`]}`)
	code, _ := post(t, ts.URL+"/v1/quoteid/batch-validate", buf.String())
	if code != 400 {
		t.Errorf("상한 초과 status=%d, want 400", code)
	}
}

func TestQuoteValidationHTTP_MarkConsumed_FirstWins(t *testing.T) {
	ts, srv := mkHTTPGateway(t)
	now := time.Now()
	_ = srv.registry.Put(context.Background(), mkRegRecord("A-1", now, time.Hour))

	code1, body1 := post(t, ts.URL+"/v1/quoteid/mark-consumed",
		`{"quoteId":"A-1","consumerId":"order-X"}`)
	if code1 != 200 {
		t.Fatalf("first: status=%d body=%s", code1, body1)
	}
	if !strings.Contains(body1, `"status":"OK"`) {
		t.Errorf("first OK 누락: %s", body1)
	}

	code2, body2 := post(t, ts.URL+"/v1/quoteid/mark-consumed",
		`{"quoteId":"A-1","consumerId":"order-Y"}`)
	if code2 != 200 {
		t.Fatalf("second: status=%d body=%s", code2, body2)
	}
	if !strings.Contains(body2, `"status":"ALREADY_CONSUMED"`) {
		t.Errorf("second ALREADY_CONSUMED 누락: %s", body2)
	}
	if !strings.Contains(body2, `"consumedBy":"order-X"`) {
		t.Errorf("consumedBy=order-X 누락: %s", body2)
	}
}

func TestQuoteValidationHTTP_Stats(t *testing.T) {
	ts, srv := mkHTTPGateway(t)
	_ = srv.registry.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))
	_, _ = post(t, ts.URL+"/v1/quoteid/validate", `{"quoteId":"A-1"}`)
	_, _ = post(t, ts.URL+"/v1/quoteid/validate", `{"quoteId":"A-xxx"}`)

	resp, err := http.Get(ts.URL + "/v1/quoteid/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stats status=%d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, `"total":2`) {
		t.Errorf("Total=2 누락: %s", body)
	}
	if !strings.Contains(body, `"ok":1`) {
		t.Errorf("OK=1 누락: %s", body)
	}
	if !strings.Contains(body, `"not_found":1`) {
		t.Errorf("NotFound=1 누락: %s", body)
	}
}
