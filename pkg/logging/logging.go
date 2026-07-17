// Package logging — WTG 전 서비스 공통 slog 초기화.
//
// 목적: cmd/*/main.go 마다 복붙되던 newLogger 를 하나로 통일하고, 출력처를
// 환경변수로 일원화한다. 초기화 한 줄이면 slog.SetDefault 까지 끝난다:
//
//	logger := logging.Init("mci-price", logging.Options{Level: *logLevelFlag})
//
// 출력처(sink) 모델 — 설정형:
//   - WTG_LOG_DIR 미설정 → stderr (EC2 systemd 가 journald 로 수집).
//   - WTG_LOG_DIR 설정   → <dir>/<svc>.log (lumberjack 회전). NH trn AP 로그
//     (~/nh-fxallone-server/win/log) 와 위치 통일용.
//
// 우선순위: Options 필드 > 환경변수 > 기본값.
//
// 환경변수:
//
//	WTG_LOG_DIR       빈값=stderr, 지정=<dir>/<svc>.log
//	WTG_LOG_LEVEL     debug|info|warn|error (기본 info)
//	WTG_LOG_FORMAT    json|text (기본 json)
//	WTG_LOG_MAX_MB    파일 회전 크기 MB (기본 100)
//	WTG_LOG_BACKUPS   보관 파일 수 (기본 10)
//	WTG_LOG_MAX_AGE   보관 일수 (기본 30)
package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// Options — 명시 설정. 빈 필드는 환경변수 → 기본값 순으로 채워진다.
type Options struct {
	Level  string // debug|info|warn|error
	Format string // json|text
	Dir    string // 로그 디렉토리. 빈값이면 stderr.
}

// resolved — env/기본값까지 적용한 최종 설정.
type resolved struct {
	level  slog.Level
	format string
	dir    string
}

// parseLevel — 레벨 문자열 → slog.Level. 알 수 없으면 Info.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// firstNonEmpty — opts 값 우선, 없으면 env, 없으면 기본.
func firstNonEmpty(optVal, envKey, dflt string) string {
	if strings.TrimSpace(optVal) != "" {
		return optVal
	}
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	return dflt
}

// resolve — Options + 환경변수 + 기본값을 합쳐 최종 설정을 만든다.
func resolve(opts Options) resolved {
	format := firstNonEmpty(opts.Format, "WTG_LOG_FORMAT", "json")
	if format != "text" {
		format = "json" // json 이외는 모두 json 으로 정규화
	}
	return resolved{
		level:  parseLevel(firstNonEmpty(opts.Level, "WTG_LOG_LEVEL", "info")),
		format: format,
		dir:    firstNonEmpty(opts.Dir, "WTG_LOG_DIR", ""),
	}
}

// envInt — 환경변수 정수 (빈값/파싱실패 시 기본).
func envInt(key string, dflt int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return dflt
}

// nopCloser — stderr 등 닫을 필요 없는 writer 용.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// buildLogger — svc 로거를 구성한다. dflt 는 파일 모드가 아닐 때 쓰는 출력처
// (실서비스는 os.Stderr, 테스트는 buffer). 반환 closer 는 파일 모드에서
// lumberjack 을 닫는다 (stderr 모드는 no-op).
func buildLogger(svc string, opts Options, dflt io.Writer) (*slog.Logger, io.Closer, error) {
	r := resolve(opts)

	var w io.Writer
	var closer io.Closer = nopCloser{}
	if r.dir != "" {
		if err := os.MkdirAll(r.dir, 0o755); err != nil {
			return nil, nopCloser{}, err
		}
		lj := &lumberjack.Logger{
			Filename:   filepath.Join(r.dir, svc+".log"),
			MaxSize:    envInt("WTG_LOG_MAX_MB", 100),
			MaxBackups: envInt("WTG_LOG_BACKUPS", 10),
			MaxAge:     envInt("WTG_LOG_MAX_AGE", 30),
			Compress:   true,
		}
		w = lj
		closer = lj
	} else {
		w = dflt
	}

	ho := &slog.HandlerOptions{Level: r.level}
	var h slog.Handler
	if r.format == "text" {
		h = slog.NewTextHandler(w, ho)
	} else {
		h = slog.NewJSONHandler(w, ho)
	}
	lg := slog.New(h).With(slog.String("svc", svc))
	return lg, closer, nil
}

// Init — svc 로거를 구성하고 slog.SetDefault 로 전역 등록한 뒤 반환한다.
// 파일 모드에서 디렉토리/파일 열기 실패 시 stderr 로 폴백한다 (기동은 막지 않음).
func Init(svc string, opts Options) *slog.Logger {
	lg, _, err := buildLogger(svc, opts, os.Stderr)
	if err != nil {
		// 파일 모드 실패 → stderr 폴백 (dir 제거).
		fallback := Options{Level: opts.Level, Format: opts.Format}
		lg, _, _ = buildLogger(svc, fallback, os.Stderr)
		lg.Warn("로그 파일 초기화 실패 — stderr 폴백", slog.Any("error", err))
	}
	slog.SetDefault(lg)
	return lg
}
