package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// HTTPPollOptions — DevMode 용 HTTP poll 기반 정책 sync.
//
// dev stack 은 etcd 없이 mci-admin / mci-api 가 각자 in-memory Engine 을
// 가져 split-brain 이 된다 — admin UI 에서 kill switch 를 토글해도 mci-api 는
// 모름. 실제 운영은 EtcdSync 가 정답이지만, dev stack 의 가벼운 대안으로
// mci-api 가 mci-admin 의 GET /v1/admin/policy 를 주기적으로 fetch 해서
// 자기 Engine 에 ApplyRemote 한다.
//
// 단방향 sync (admin → api). admin 이 source of truth.
type HTTPPollOptions struct {
	URL      string            // 예: http://127.0.0.1:9090/v1/admin/policy
	Interval time.Duration     // default 2s
	Headers  map[string]string // 인증 헤더 (DevMode: X-WTG-User=dev-poller)
	Logger   *slog.Logger
}

// StartHTTPPoll 은 goroutine 을 띄워 주기적으로 URL 을 fetch → State 파싱 →
// engine.ApplyRemote. ctx 종료 시 자연 종료.
//
// 운영에서는 호출하지 않는다 (etcd 가 진실의 원천).
func StartHTTPPoll(ctx context.Context, engine *Engine, opt HTTPPollOptions) error {
	if engine == nil {
		return errors.New("policy: Engine 필수")
	}
	if opt.URL == "" {
		return errors.New("policy: URL 필수")
	}
	if opt.Interval <= 0 {
		opt.Interval = 2 * time.Second
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}

	go pollLoop(ctx, engine, opt)
	return nil
}

func pollLoop(ctx context.Context, engine *Engine, opt HTTPPollOptions) {
	client := &http.Client{Timeout: 3 * time.Second}
	t := time.NewTicker(opt.Interval)
	defer t.Stop()

	opt.Logger.Info("policy HTTP poll sync 시작",
		slog.String("url", opt.URL),
		slog.Duration("interval", opt.Interval))

	// 즉시 한 번 — 부팅 시 admin 의 현재 상태를 빨리 받는다.
	pollOnce(ctx, engine, client, opt)

	for {
		select {
		case <-ctx.Done():
			opt.Logger.Info("policy HTTP poll sync 종료", slog.String("url", opt.URL))
			return
		case <-t.C:
			pollOnce(ctx, engine, client, opt)
		}
	}
}

func pollOnce(ctx context.Context, engine *Engine, client *http.Client, opt HTTPPollOptions) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opt.URL, nil)
	if err != nil {
		// URL 은 한 번 검증되었으므로 사실상 도달 불가.
		return
	}
	for k, v := range opt.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		// 네트워크 일시 단절은 흔한 상황 — debug level 로 강등.
		opt.Logger.Debug("policy poll 실패", slog.Any("err", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		opt.Logger.Warn("policy poll non-200",
			slog.Int("status", resp.StatusCode))
		return
	}

	// admin 의 GET /v1/admin/policy 는 State JSON 을 직접 반환 (감싸지 않음).
	var st State
	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		opt.Logger.Warn("policy poll 응답 파싱 실패", slog.Any("err", err))
		return
	}

	// Maintenance 의 zero time 은 비활성으로 처리되므로 그대로 적용 OK.
	engine.ApplyRemote(st)
}

// SanitizePollURL — 사용자 입력의 trailing slash 제거 등 단순 정리.
// scheme 검증은 하지 않는다 (config 단계에서 책임).
func SanitizePollURL(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
	}
	return s
}

// DescribeState — 디버그용 한 줄 요약 (테스트/로그에서 활용).
func DescribeState(s State) string {
	parts := []string{}
	if s.KillSwitch {
		parts = append(parts, "kill_switch=on")
	}
	if !s.Maintenance.Start.IsZero() && !s.Maintenance.End.IsZero() {
		parts = append(parts, fmt.Sprintf("maintenance=%s..%s",
			s.Maintenance.Start.Format(time.RFC3339), s.Maintenance.End.Format(time.RFC3339)))
	}
	if len(s.BlockedSymbols) > 0 {
		parts = append(parts, "blocked_symbols="+strings.Join(s.BlockedSymbols, ","))
	}
	if len(s.BlockedRoutingKeys) > 0 {
		parts = append(parts, "blocked_rkeys="+strings.Join(s.BlockedRoutingKeys, ","))
	}
	if len(parts) == 0 {
		return "clean"
	}
	return strings.Join(parts, " ")
}
