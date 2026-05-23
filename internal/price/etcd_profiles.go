package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/session"
)

// EtcdProfileSource 는 etcd prefix 아래의 활성 Profile 카탈로그를 watch 해서
// PricingConsumer 의 ProfileSource 인터페이스를 만족한다.
//
// 패턴:
//
//   - prefix (예: "wtg/price/profiles/") 아래 각 key 는 단일 Profile.
//     value 는 session.Profile JSON.
//   - 시작 시 prefix Get → 누적 → atomic snapshot 저장.
//   - 백그라운드 watch → PUT/DELETE 이벤트 → snapshot 재빌드 + atomic swap.
//   - hot path (ActiveProfiles) 는 atomic.Pointer 만 Load — lock 없음.
type EtcdProfileSource struct {
	cli    *clientv3.Client
	prefix string
	logger *slog.Logger

	mu      sync.Mutex
	entries map[string]session.Profile // etcd key → profile

	snap atomic.Pointer[[]session.Profile]

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdProfileSourceOptions 는 EtcdProfileSource 생성 옵션.
type EtcdProfileSourceOptions struct {
	Client *clientv3.Client // 호출자가 dial 한 etcd 클라이언트. 필수.
	Prefix string           // default "wtg/price/profiles/"
	Logger *slog.Logger
}

// NewEtcdProfileSource 는 1회 로드 + watch 시작.
func NewEtcdProfileSource(ctx context.Context, opt EtcdProfileSourceOptions) (*EtcdProfileSource, error) {
	if opt.Client == nil {
		return nil, errors.New("price: etcd Client 필수")
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg/price/profiles/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &EtcdProfileSource{
		cli:     opt.Client,
		prefix:  prefix,
		logger:  logger,
		entries: make(map[string]session.Profile),
		stopC:   make(chan struct{}),
		doneC:   make(chan struct{}),
	}
	// 빈 snapshot 으로 초기화 — 부트 직후에도 ActiveProfiles 가 안전.
	empty := []session.Profile{}
	s.snap.Store(&empty)

	if err := s.initialLoad(ctx); err != nil {
		return nil, err
	}
	go s.watchLoop(ctx)
	return s, nil
}

// ActiveProfiles 는 ProfileSource 인터페이스 구현 — atomic snapshot 의 복사본.
func (s *EtcdProfileSource) ActiveProfiles() []session.Profile {
	p := s.snap.Load()
	if p == nil {
		return nil
	}
	out := make([]session.Profile, len(*p))
	copy(out, *p)
	return out
}

func (s *EtcdProfileSource) initialLoad(ctx context.Context) error {
	resp, err := s.cli.Get(ctx, s.prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("price: profile etcd 초기 Get: %w", err)
	}
	s.mu.Lock()
	for _, kv := range resp.Kvs {
		var p session.Profile
		if err := json.Unmarshal(kv.Value, &p); err != nil {
			s.logger.Warn("Profile JSON 파싱 실패 (skip)",
				slog.String("key", string(kv.Key)),
				slog.Any("error", err),
			)
			continue
		}
		s.entries[string(kv.Key)] = p
	}
	s.mu.Unlock()
	s.rebuildSnapshot()
	s.logger.Info("ProfileSource etcd 초기 로드",
		slog.String("prefix", s.prefix),
		slog.Int("count", len(s.ActiveProfiles())),
	)
	return nil
}

func (s *EtcdProfileSource) watchLoop(ctx context.Context) {
	defer close(s.doneC)
	wch := s.cli.Watch(ctx, s.prefix, clientv3.WithPrefix())
	for {
		select {
		case <-s.stopC:
			return
		case <-ctx.Done():
			return
		case wresp, ok := <-wch:
			if !ok {
				s.logger.Warn("ProfileSource watch 채널 종료 — 재등록")
				wch = s.cli.Watch(ctx, s.prefix, clientv3.WithPrefix())
				continue
			}
			if err := wresp.Err(); err != nil {
				s.logger.Warn("ProfileSource watch 에러", slog.Any("error", err))
				continue
			}
			s.applyEvents(wresp.Events)
		}
	}
}

func (s *EtcdProfileSource) applyEvents(events []*clientv3.Event) {
	if len(events) == 0 {
		return
	}
	s.mu.Lock()
	for _, ev := range events {
		key := string(ev.Kv.Key)
		switch ev.Type {
		case clientv3.EventTypePut:
			var p session.Profile
			if err := json.Unmarshal(ev.Kv.Value, &p); err != nil {
				s.logger.Warn("Profile PUT 파싱 실패",
					slog.String("key", key),
					slog.Any("error", err),
				)
				continue
			}
			s.entries[key] = p
		case clientv3.EventTypeDelete:
			delete(s.entries, key)
		}
	}
	s.mu.Unlock()
	s.rebuildSnapshot()
}

func (s *EtcdProfileSource) rebuildSnapshot() {
	s.mu.Lock()
	list := make([]session.Profile, 0, len(s.entries))
	for _, p := range s.entries {
		list = append(list, p)
	}
	s.mu.Unlock()
	s.snap.Store(&list)
}

// Close 는 watch goroutine 종료 (idempotent). etcd client 는 호출자 관리.
func (s *EtcdProfileSource) Close() error {
	s.stopOnce.Do(func() {
		close(s.stopC)
		<-s.doneC
	})
	return nil
}

// 컴파일 타임: ProfileSource 인터페이스 구현 보장.
var _ ProfileSource = (*EtcdProfileSource)(nil)
