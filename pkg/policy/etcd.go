package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdSync 는 단일 etcd key 에 정책 State 를 JSON 으로 저장하고,
// watch 로 다른 인스턴스의 변경을 받아 Engine 에 적용한다.
//
// 토폴로지: mci-admin 이 변경 → etcd Put → mci-api 인스턴스들이 watch 받음 → ApplyRemote.
//
// 정책은 상태가 단일 객체이고 변경 빈도가 낮으므로 routing 처럼 prefix Get 이
// 아니라 단일 key 통째로 다룬다 — 단순 + atomic.
type EtcdSync struct {
	cli    *clientv3.Client
	key    string
	engine *Engine
	logger *slog.Logger

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdSyncOptions 는 EtcdSync 생성 옵션.
type EtcdSyncOptions struct {
	Endpoints   []string
	Key         string // default "wtg/policy"
	DialTimeout time.Duration
	Username    string
	Password    string
	Logger      *slog.Logger
}

// StartEtcdSync 는 etcd 클라이언트를 dial 하고 초기 Get 으로 Engine 을 채운 뒤,
// watch goroutine 을 띄우고 Engine.OnChange 로 etcd Put 을 등록한다.
//
// ctx 는 watch 의 lifetime 을 정의 — 호출자 종료 시 cancel 권장 (Close 도 별도).
func StartEtcdSync(ctx context.Context, engine *Engine, opt EtcdSyncOptions) (*EtcdSync, error) {
	if engine == nil {
		return nil, errors.New("policy: Engine 필수")
	}
	if len(opt.Endpoints) == 0 {
		return nil, errors.New("policy: etcd Endpoints 필수")
	}
	if opt.DialTimeout == 0 {
		opt.DialTimeout = 5 * time.Second
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   opt.Endpoints,
		DialTimeout: opt.DialTimeout,
		Username:    opt.Username,
		Password:    opt.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("policy: etcd dial: %w", err)
	}
	key := opt.Key
	if key == "" {
		key = "wtg/policy"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &EtcdSync{
		cli:    cli,
		key:    key,
		engine: engine,
		logger: logger,
		stopC:  make(chan struct{}),
		doneC:  make(chan struct{}),
	}
	if err := s.initialLoad(ctx); err != nil {
		_ = cli.Close()
		return nil, err
	}

	// 변경 시 etcd persist — 단, ApplyRemote 호출 시에는 callback suppression 으로
	// 호출되지 않아 self-loop 방지. AddOnChange 로 다중 sink 호환.
	engine.AddOnChange(s.persist)

	go s.watchLoop(ctx)
	return s, nil
}

// initialLoad — etcd 에 기존 정책이 있으면 Engine 에 ApplyRemote.
func (s *EtcdSync) initialLoad(ctx context.Context) error {
	resp, err := s.cli.Get(ctx, s.key)
	if err != nil {
		return fmt.Errorf("policy: etcd 초기 Get: %w", err)
	}
	if len(resp.Kvs) == 0 {
		s.logger.Info("EtcdSync 초기 — etcd 에 정책 없음, Engine 기본값 유지")
		return nil
	}
	var st State
	if err := json.Unmarshal(resp.Kvs[0].Value, &st); err != nil {
		s.logger.Warn("EtcdSync 초기 파싱 실패", slog.Any("error", err))
		return nil // 무시 — Engine 기본값으로 진행
	}
	s.engine.ApplyRemote(st)
	s.logger.Info("EtcdSync 초기 로드", slog.String("key", s.key),
		slog.Bool("kill_switch", st.KillSwitch),
		slog.Int("blocked_symbols", len(st.BlockedSymbols)),
	)
	return nil
}

// persist — Engine.OnChange 콜백. 변경된 State 를 etcd 에 Put.
func (s *EtcdSync) persist(st State) {
	value, err := json.Marshal(&st)
	if err != nil {
		s.logger.Error("policy: persist marshal 실패", slog.Any("error", err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.cli.Put(ctx, s.key, string(value)); err != nil {
		s.logger.Error("policy: etcd persist 실패", slog.String("key", s.key), slog.Any("error", err))
		return
	}
}

// watchLoop — etcd watch 로 외부 인스턴스 변경 감지 → Engine.ApplyRemote.
//
// 자기가 Put 한 변경도 받지만, ApplyRemote 가 callback 을 suppress 하므로
// 무한 루프는 발생하지 않는다 (cost 는 idempotent JSON unmarshal 한 번).
func (s *EtcdSync) watchLoop(ctx context.Context) {
	defer close(s.doneC)
	wch := s.cli.Watch(ctx, s.key)
	for {
		select {
		case <-s.stopC:
			return
		case <-ctx.Done():
			return
		case wresp, ok := <-wch:
			if !ok {
				s.logger.Warn("EtcdSync watch 채널 종료 — 재등록")
				wch = s.cli.Watch(ctx, s.key)
				continue
			}
			if err := wresp.Err(); err != nil {
				s.logger.Warn("EtcdSync watch 에러", slog.Any("error", err))
				continue
			}
			for _, ev := range wresp.Events {
				switch ev.Type {
				case clientv3.EventTypePut:
					var st State
					if err := json.Unmarshal(ev.Kv.Value, &st); err != nil {
						s.logger.Warn("policy: watch put 파싱 실패", slog.Any("error", err))
						continue
					}
					s.engine.ApplyRemote(st)
				case clientv3.EventTypeDelete:
					// 정책 키 삭제 → 빈 상태 적용 (모두 허용).
					s.engine.ApplyRemote(State{})
				}
			}
		}
	}
}

// Close — watch 정리, etcd 클라이언트 종료.
//
// 주의: Engine 의 콜백 슬라이스에서 자기 callback 만 제거하지는 않는다 — 종료 시점에
// 어차피 Engine 도 사용 종료되는 라이프사이클이라 단순화. 필요하면 별도 RemoveOnChange 추가.
func (s *EtcdSync) Close() error {
	s.stopOnce.Do(func() {
		close(s.stopC)
		select {
		case <-s.doneC:
		case <-time.After(2 * time.Second):
		}
	})
	return s.cli.Close()
}

// SplitEndpoints — 콤마 구분 endpoints 문자열을 슬라이스로.
// pkg/routing 의 동일 헬퍼와 별도로 두는 이유: 의존성 격리.
func SplitEndpoints(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
