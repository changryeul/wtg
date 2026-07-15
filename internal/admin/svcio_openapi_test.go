package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/winwaysystems/wtg/pkg/svcio"
)

// 최소 유효 헤더 — svcio 파서가 Input/Output 구조를 뽑을 수 있는 형태.
const testHdrW9999S01 = `#ifndef __W9999S01__H__
#define __W9999S01__H__
typedef struct {  // Input
	char code[10];  // 코드
} W9999S01_I;
typedef struct {  // Output
	char rcnt[6];   // 건수
} W9999S01_O;
#endif
`

func newTestSvcRegistry(t *testing.T) *svcio.Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "W9999S01.h"), []byte(testHdrW9999S01), 0o644); err != nil {
		t.Fatal(err)
	}
	r := svcio.NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, _, err := r.LoadDir(dir, logger); err != nil {
		t.Fatal(err)
	}
	if r.Count() == 0 {
		t.Fatal("헤더 로드 실패 — spec 0개")
	}
	return r
}

func fetchOpenAPIServers(t *testing.T, deps *SvcIODeps) []svcio.OpenAPIServer {
	return fetchOpenAPIServersQ(t, deps, "")
}

func fetchOpenAPIServersQ(t *testing.T, deps *SvcIODeps, query string) []svcio.OpenAPIServer {
	t.Helper()
	u := "http://admin.example:9090/v1/admin/svc-io/openapi.json"
	if query != "" {
		u += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	rec := httptest.NewRecorder()
	GetSvcIOOpenAPI(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var doc svcio.OpenAPIDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("openapi json unmarshal: %v", err)
	}
	return doc.Servers
}

// DevMode 면 외부 DMZ 서버(OpenAPIServer) + 관리자 콘솔 same-origin 테스트
// 서버 2개가 등록되어야 한다 (Swagger UI "Try it out" 우회용).
func TestGetSvcIOOpenAPI_DevModeAddsTestServer(t *testing.T) {
	deps := &SvcIODeps{
		Registry:      newTestSvcRegistry(t),
		OpenAPIServer: "https://3.36.188.87:8090",
		DevMode:       true,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	servers := fetchOpenAPIServers(t, deps)
	if len(servers) != 2 {
		t.Fatalf("servers %d개, want 2: %+v", len(servers), servers)
	}
	if servers[0].URL != "https://3.36.188.87:8090" {
		t.Fatalf("servers[0]=%q, want 외부 DMZ", servers[0].URL)
	}
	if servers[1].URL != "http://admin.example:9090" {
		t.Fatalf("servers[1]=%q, want 요청 origin (same-origin)", servers[1].URL)
	}
}

// try=1 (viewer 모드) 이면 same-origin 테스트 서버를 servers[0] 로 앞세운다 —
// Swagger UI 기본 선택이 테스트 서버가 되어 Try it out 이 바로 동작.
func TestGetSvcIOOpenAPI_TryModeTestServerFirst(t *testing.T) {
	deps := &SvcIODeps{
		Registry:      newTestSvcRegistry(t),
		OpenAPIServer: "https://3.36.188.87:8090",
		DevMode:       true,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	servers := fetchOpenAPIServersQ(t, deps, "try=1")
	if len(servers) != 2 {
		t.Fatalf("servers %d개, want 2: %+v", len(servers), servers)
	}
	if servers[0].URL != "http://admin.example:9090" {
		t.Fatalf("servers[0]=%q, want same-origin 테스트 서버 우선", servers[0].URL)
	}
	if servers[1].URL != "https://3.36.188.87:8090" {
		t.Fatalf("servers[1]=%q, want 외부 DMZ", servers[1].URL)
	}
}

// 비 DevMode 면 외부 DMZ 서버 1개만 (테스트 서버 미등록).
func TestGetSvcIOOpenAPI_NonDevModeSingleServer(t *testing.T) {
	deps := &SvcIODeps{
		Registry:      newTestSvcRegistry(t),
		OpenAPIServer: "https://3.36.188.87:8090",
		DevMode:       false,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	servers := fetchOpenAPIServers(t, deps)
	if len(servers) != 1 {
		t.Fatalf("servers %d개, want 1: %+v", len(servers), servers)
	}
}
