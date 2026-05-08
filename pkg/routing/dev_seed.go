package routing

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"time"
)

// SeedPolicy — cfg 파일과 in-memory registry 간 동기화 정책.
//
//   - PolicyAdditive (기본): cfg 의 alias 가 in-memory 에 없으면 추가만 한다.
//     기존 alias (cfg 에서 변경되었거나 cfg 에서 사라진 것 모두) 는 손대지 않는다.
//     UI 에서 사용자가 즉석으로 추가한 룰을 보존하는 데 유리하다.
//
//   - PolicySync: cfg 가 진실의 원천. (a) cfg 의 모든 alias 를 upsert (변경 필드도
//     덮어쓰기), (b) cfg 에 없는 in-memory alias 는 삭제.
//     wtgctl routes del/set 이 즉시 in-memory 에 반영된다. UI 만으로 추가한 룰은
//     hot reload 시 사라지므로, 운영자가 의식적으로 선택할 때 사용한다.
type SeedPolicy string

const (
	PolicyAdditive SeedPolicy = "additive"
	PolicySync     SeedPolicy = "sync"
)

// ParseSeedPolicy 는 문자열을 SeedPolicy 로 변환. 빈 문자열은 Additive default.
// 알 수 없는 값은 error 반환 — caller 가 cfg 검증 단계에서 거부할 수 있게 한다.
func ParseSeedPolicy(s string) (SeedPolicy, error) {
	switch s {
	case "", string(PolicyAdditive):
		return PolicyAdditive, nil
	case string(PolicySync):
		return PolicySync, nil
	default:
		return "", errors.New("routing: unknown seed policy " + s + " (allowed: additive|sync)")
	}
}

// 기본 hardcode 시드 — 외부 cfg 파일이 없거나 읽기 실패 시 fallback.
// dev stack 의 broker 컨테이너 entrypoint 가 자동 기동하는 service 들의 alias.
var defaultDevSeeds = []Rule{
	{Alias: "TSTSVC_PING", Exchange: "TSTSVC", RoutingKey: "PING", Active: true,
		Comment: "broker entrypoint 의 test_service (single rkey)"},
	{Alias: "WECHO_PING", Exchange: "ECHOSVC", RoutingKey: "PING", Active: true,
		Comment: "WECHO PING → PONG"},
	{Alias: "WECHO_ECHO", Exchange: "ECHOSVC", RoutingKey: "ECHO", Active: true,
		Comment: "WECHO ECHO → 'ECHO:'+payload"},
	{Alias: "WECHO_UPPER", Exchange: "ECHOSVC", RoutingKey: "UPPER", Active: true,
		Comment: "WECHO UPPER → 대문자 변환"},
	{Alias: "WECHO_TIME", Exchange: "ECHOSVC", RoutingKey: "TIME", Active: true,
		Comment: "WECHO TIME → YYYYMMDDHHMMSS"},
	{Alias: "WECHO_INFO", Exchange: "ECHOSVC", RoutingKey: "INFO", Active: true,
		Comment: "WECHO INFO → pid + uptime"},
}

// LoadRoutesFromFile 은 JSON 파일에서 Rule 배열을 로드한다. JSON 형식:
//
//	[ {"alias":"...","exchange":"...","routing_key":"...","active":true,"comment":"..."}, ... ]
//	또는
//	{ "routes": [ ... ] }
//
// 파일이 없거나 읽기 실패 시 (rules=nil, err) 반환. 호출자가 default fallback 결정.
func LoadRoutesFromFile(path string) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// 두 형식 모두 지원: top-level array 또는 {"routes": [...]}.
	var arr []Rule
	if err := json.Unmarshal(data, &arr); err == nil {
		return arr, nil
	}
	var wrap struct {
		Routes []Rule `json:"routes"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, err
	}
	return wrap.Routes, nil
}

// SeedDevRoutes — 기존 호출부 호환 (hardcode default 만 시드, additive).
// 새 코드는 SeedDevRoutesExPolicy 를 권장.
func SeedDevRoutes(reg Registry, logger *slog.Logger) {
	applyRules(reg, logger, defaultDevSeeds, "default", PolicyAdditive)
}

// SeedDevRoutesEx — additive policy 로 위임 (BC).
func SeedDevRoutesEx(reg Registry, logger *slog.Logger, path string) {
	SeedDevRoutesExPolicy(reg, logger, path, PolicyAdditive)
}

// SeedDevRoutesExPolicy — cfg 파일 우선, 없으면 hardcode default 로 fallback.
// policy 에 따라 in-memory 갱신 전략이 달라진다 (SeedPolicy 주석 참조).
//
// path 가 비어있거나 파일 부재면 default. 파일 있어도 파싱 실패면 default 로
// fallback (운영 안정성 — cfg 오타가 dev stack 부팅을 망가뜨리지 않도록).
// 단, sync 모드에서 cfg 파일 파싱 실패는 default 로 fallback 하지 않는다 —
// sync 약속이 깨지므로, 안전한 additive 로 처리한 뒤 경고 로그.
//
// 파일 위치 권장: ~/mymq/etc/wtg-routes.json (broker cfg 와 같은 운영 디렉터리).
func SeedDevRoutesExPolicy(reg Registry, logger *slog.Logger, path string, policy SeedPolicy) {
	if path == "" {
		applyRules(reg, logger, defaultDevSeeds, "default", policy)
		return
	}
	rules, err := LoadRoutesFromFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Info("dev-seed cfg 파일 없음 — default 사용",
				slog.String("path", path), slog.Int("default_count", len(defaultDevSeeds)))
		} else {
			logger.Warn("dev-seed cfg 파일 읽기 실패 — default fallback (sync 약속 미적용)",
				slog.String("path", path), slog.Any("err", err))
		}
		// 파싱 실패 시엔 sync 의 "cfg 가 진실의 원천" 약속을 지킬 수 없으므로
		// 강제로 additive 로 fallback. cfg 가 정상화되면 다음 reload 에서 정상 동작.
		applyRules(reg, logger, defaultDevSeeds, "default", PolicyAdditive)
		return
	}
	applyRules(reg, logger, rules, "file:"+path, policy)
}

// WatchRoutesFile — additive policy 로 위임 (BC).
func WatchRoutesFile(ctx context.Context, reg Registry, logger *slog.Logger, path string, interval time.Duration) {
	WatchRoutesFilePolicy(ctx, reg, logger, path, interval, PolicyAdditive)
}

// WatchRoutesFilePolicy 는 cfg 파일의 mtime 을 polling 해서 변경 시마다 재시드.
// fsnotify 의존성을 피하기 위해 stat polling (tlsutil/reloader.go 와 같은 패턴).
//
// goroutine 으로 실행 — ctx 취소 시 종료. interval 0 또는 path 빈값이면 noop.
// 정책 의미는 SeedPolicy 주석 참조.
func WatchRoutesFilePolicy(ctx context.Context, reg Registry, logger *slog.Logger, path string, interval time.Duration, policy SeedPolicy) {
	if path == "" || interval <= 0 {
		return
	}
	go func() {
		var lastMod time.Time
		// 첫 stat — 초기 mtime 기록 (시드는 server.go 가 부팅 시 따로 호출하므로
		// 여기서 다시 시드하지 않는다).
		if st, err := os.Stat(path); err == nil {
			lastMod = st.ModTime()
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		logger.Info("dev-routes watcher 시작",
			slog.String("path", path),
			slog.Duration("interval", interval),
			slog.String("policy", string(policy)))
		for {
			select {
			case <-ctx.Done():
				logger.Info("dev-routes watcher 종료", slog.String("path", path))
				return
			case <-ticker.C:
				st, err := os.Stat(path)
				if err != nil {
					// 파일 사라짐 — 다음 tick 까지 무시 (운영자가 cfg 재배포 중일 수도)
					continue
				}
				if !st.ModTime().After(lastMod) {
					continue
				}
				lastMod = st.ModTime()
				logger.Info("dev-routes cfg 변경 감지 — 재시드",
					slog.String("path", path),
					slog.Time("mtime", lastMod),
					slog.String("policy", string(policy)))
				SeedDevRoutesExPolicy(reg, logger, path, policy)
			}
		}
	}()
}

// applyRules — policy 에 따라 in-memory registry 를 갱신.
//
//   - additive: 기존 alias 가 있으면 skip (변경 무시), 없는 것만 Put.
//     cfg 에서 사라진 alias 도 in-memory 유지.
//   - sync: 모든 cfg alias 를 upsert (변경 필드 덮어쓰기). cfg 에 없는
//     in-memory alias 는 Delete.
func applyRules(reg Registry, logger *slog.Logger, seeds []Rule, source string, policy SeedPolicy) {
	const updatedBy = "dev-seed"
	added, updated, skipped, removed := 0, 0, 0, 0

	// cfg 에 있는 alias set — sync 모드에서 prune 대상 판별용.
	cfgAliases := make(map[string]struct{}, len(seeds))
	for i := range seeds {
		cfgAliases[seeds[i].Alias] = struct{}{}
	}

	for i := range seeds {
		s := seeds[i]
		existing, err := reg.Get(s.Alias)
		if err != nil && !errors.Is(err, ErrRouteNotFound) {
			logger.Warn("dev-seed Get 실패",
				slog.String("alias", s.Alias), slog.Any("err", err))
			continue
		}
		switch policy {
		case PolicySync:
			// upsert — 변경 필드도 덮어쓰기. UpdatedAt 은 Put 이 갱신.
			if err := reg.Put(&s, updatedBy); err != nil {
				logger.Warn("dev-seed Put 실패",
					slog.String("alias", s.Alias), slog.Any("err", err))
				continue
			}
			if existing == nil {
				added++
			} else {
				updated++
			}
		default: // additive
			if existing != nil {
				skipped++
				continue
			}
			if err := reg.Put(&s, updatedBy); err != nil {
				logger.Warn("dev-seed Put 실패",
					slog.String("alias", s.Alias), slog.Any("err", err))
				continue
			}
			added++
		}
	}

	// sync 모드: cfg 에 없는 in-memory alias 제거.
	if policy == PolicySync {
		for _, r := range reg.List() {
			if _, ok := cfgAliases[r.Alias]; ok {
				continue
			}
			if err := reg.Delete(r.Alias); err != nil {
				logger.Warn("dev-seed Delete 실패",
					slog.String("alias", r.Alias), slog.Any("err", err))
				continue
			}
			removed++
		}
	}

	logger.Info("dev-seed 라우팅 룰",
		slog.String("source", source),
		slog.String("policy", string(policy)),
		slog.Int("added", added),
		slog.Int("updated", updated),
		slog.Int("skipped_existing", skipped),
		slog.Int("removed", removed),
		slog.Int("total_cfg", len(seeds)),
	)
}
