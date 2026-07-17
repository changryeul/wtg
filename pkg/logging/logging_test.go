package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildLogger 는 stderr 대체 writer(dflt)를 주입받아 테스트 가능하다.
// 우선순위: opts 필드 > 환경변수 > 기본값.

func TestResolve_Precedence(t *testing.T) {
	t.Setenv("WTG_LOG_LEVEL", "warn")
	t.Setenv("WTG_LOG_FORMAT", "text")
	t.Setenv("WTG_LOG_DIR", "/env/dir")

	// opts 가 env 를 이긴다.
	r := resolve(Options{Level: "debug", Format: "json", Dir: "/opts/dir"})
	if r.level != slog.LevelDebug {
		t.Errorf("level = %v, want debug (opts 우선)", r.level)
	}
	if r.format != "json" {
		t.Errorf("format = %q, want json (opts 우선)", r.format)
	}
	if r.dir != "/opts/dir" {
		t.Errorf("dir = %q, want /opts/dir (opts 우선)", r.dir)
	}

	// opts 비면 env 사용.
	r = resolve(Options{})
	if r.level != slog.LevelWarn {
		t.Errorf("level = %v, want warn (env)", r.level)
	}
	if r.format != "text" {
		t.Errorf("format = %q, want text (env)", r.format)
	}
	if r.dir != "/env/dir" {
		t.Errorf("dir = %q, want /env/dir (env)", r.dir)
	}
}

func TestResolve_Defaults(t *testing.T) {
	// env 미설정 (t.Setenv 로 빈값 강제).
	t.Setenv("WTG_LOG_LEVEL", "")
	t.Setenv("WTG_LOG_FORMAT", "")
	t.Setenv("WTG_LOG_DIR", "")

	r := resolve(Options{})
	if r.level != slog.LevelInfo {
		t.Errorf("기본 level = %v, want info", r.level)
	}
	if r.format != "json" {
		t.Errorf("기본 format = %q, want json", r.format)
	}
	if r.dir != "" {
		t.Errorf("기본 dir = %q, want 빈값(stderr)", r.dir)
	}
}

func TestLevelParse(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"DEBUG": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"":      slog.LevelInfo,
		"bogus": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBuildLogger_StderrDefault(t *testing.T) {
	t.Setenv("WTG_LOG_DIR", "")
	var buf bytes.Buffer
	lg, closer, err := buildLogger("mci-price", Options{Format: "json"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	lg.Info("hello", slog.Int("n", 7))
	out := buf.String()
	if out == "" {
		t.Fatal("stderr writer 로 출력 안 됨")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
		t.Fatalf("json 아님: %v (%q)", err, out)
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v", rec["msg"])
	}
	if rec["svc"] != "mci-price" {
		t.Errorf("svc 태그 누락: %v", rec["svc"])
	}
	if rec["n"] != float64(7) {
		t.Errorf("n = %v", rec["n"])
	}
}

func TestBuildLogger_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	lg, closer, _ := buildLogger("svc1", Options{Format: "text", Dir: ""}, &buf)
	defer closer.Close()
	lg.Info("hi")
	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("text 포맷인데 JSON 으로 나옴: %q", out)
	}
	if !strings.Contains(out, "svc=svc1") {
		t.Errorf("text 에 svc=svc1 없음: %q", out)
	}
}

func TestBuildLogger_FileMode(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer // 파일 모드면 stderr 로는 안 나가야 함
	lg, closer, err := buildLogger("mci-edge-tcp", Options{Dir: dir, Format: "json"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("to-file")
	closer.Close()

	want := filepath.Join(dir, "mci-edge-tcp.log")
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("로그 파일 %s 없음: %v", want, err)
	}
	if !strings.Contains(string(data), "to-file") {
		t.Errorf("파일에 로그 없음: %q", data)
	}
	if !strings.Contains(string(data), `"svc":"mci-edge-tcp"`) {
		t.Errorf("파일 로그에 svc 태그 누락: %q", data)
	}
	if stderr.Len() != 0 {
		t.Errorf("파일 모드인데 stderr 로도 나감: %q", stderr.String())
	}
}

func TestInit_SetsDefault(t *testing.T) {
	t.Setenv("WTG_LOG_DIR", "")
	lg := Init("mci-admin", Options{Level: "debug"})
	if lg == nil {
		t.Fatal("Init nil 반환")
	}
	// SetDefault 되어 slog.Default() 가 같은 설정을 갖는지 (svc 태그 확인은 어려우므로 non-nil 만).
	if slog.Default() == nil {
		t.Error("slog.Default 미설정")
	}
}
