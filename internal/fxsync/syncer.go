package fxsync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Syncer — Backend 의 마스터 데이터를 etcd 의 wtg/* 키 공간으로 PUT.
//
// 정책:
//   - 각 entry 가 별도 key (예: wtg/currency/{code}). 변경/추가만 PUT,
//     etcd 의 stale 키 (DB 에는 없는데 etcd 에만 있는) 는 옵션 (DeleteStale)
//     에 따라 삭제.
//   - JSON encode 는 도메인 struct 그대로 (DB-mirror 도 같은 형식).
//   - Active=false entry 는 etcd 에 PUT 안 함 (사실상 삭제).
type Syncer struct {
	Etcd         *clientv3.Client
	Prefix       string // 기본 "wtg/"
	DeleteStale  bool   // 기본 true — DB 에 없는 etcd 키 정리
	Logger       *slog.Logger
}

// NewSyncer — 기본 옵션으로 생성. Prefix 빈값이면 "wtg/".
func NewSyncer(cli *clientv3.Client, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{
		Etcd:        cli,
		Prefix:      "wtg/",
		DeleteStale: true,
		Logger:      logger,
	}
}

// SyncResult — 한 테이블 sync 결과. 운영 로그 / admin UI 응답에 사용.
type SyncResult struct {
	Table       string `json:"table"`
	SourceCount int    `json:"source_count"`  // backend 가 read 한 row 수
	Active      int    `json:"active"`        // Active=true 만
	Put         int    `json:"put"`           // etcd 에 PUT 한 수
	DeletedStale int   `json:"deleted_stale"` // 정리한 stale 키 수
}

// SyncCurrencies — Currency 테이블 sync. wtg/currency/{code} 에 PUT.
func (s *Syncer) SyncCurrencies(ctx context.Context, currencies Currencies) (SyncResult, error) {
	r := SyncResult{Table: "currency", SourceCount: len(currencies)}
	keyPrefix := s.Prefix + "currency/"

	// 1. 활성 entry 만 추려 PUT 대상 set 구성.
	want := make(map[string]Currency, len(currencies))
	for _, c := range currencies {
		if !c.Active {
			continue
		}
		if c.Code == "" {
			continue
		}
		want[c.Code] = c
		r.Active++
	}

	// 2. 기존 etcd 키 수집 (stale 비교용).
	existing := map[string]struct{}{}
	if s.DeleteStale {
		resp, err := s.Etcd.Get(ctx, keyPrefix, clientv3.WithPrefix(), clientv3.WithKeysOnly())
		if err != nil {
			return r, fmt.Errorf("fxsync: list %s: %w", keyPrefix, err)
		}
		for _, kv := range resp.Kvs {
			code := strings.TrimPrefix(string(kv.Key), keyPrefix)
			existing[code] = struct{}{}
		}
	}

	// 3. PUT.
	for code, cur := range want {
		body, err := json.Marshal(cur)
		if err != nil {
			return r, fmt.Errorf("fxsync: marshal %s: %w", code, err)
		}
		if _, err := s.Etcd.Put(ctx, keyPrefix+code, string(body)); err != nil {
			return r, fmt.Errorf("fxsync: put %s: %w", code, err)
		}
		r.Put++
		delete(existing, code)
	}

	// 4. stale 삭제.
	if s.DeleteStale {
		for code := range existing {
			if _, err := s.Etcd.Delete(ctx, keyPrefix+code); err != nil {
				s.Logger.Warn("fxsync: delete stale 실패",
					slog.String("code", code), slog.Any("error", err))
				continue
			}
			r.DeletedStale++
		}
	}

	s.Logger.Info("fxsync: currency sync 완료",
		slog.Int("source", r.SourceCount),
		slog.Int("active", r.Active),
		slog.Int("put", r.Put),
		slog.Int("deleted_stale", r.DeletedStale),
	)
	return r, nil
}
