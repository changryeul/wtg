package routing

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// FactoryOptions 는 endpoints/prefix 만으로 Registry 를 만드는 편의 옵션.
type FactoryOptions struct {
	// Endpoints 가 비면 InMemoryRegistry 반환 (test/dev/단일 인스턴스).
	Endpoints string // 콤마 구분 (예: "etcd-0:2379,etcd-1:2379")
	Prefix    string // etcd key prefix
	Logger    *slog.Logger
}

// New 는 endpoints 가 채워져 있으면 EtcdRegistry, 비면 InMemoryRegistry 를 만든다.
//
// 호출자 (mci-api / mci-admin) 가 운영 옵션 분기를 한 곳에서 처리할 수 있게 하는 편의.
func New(ctx context.Context, opt FactoryOptions) (Registry, error) {
	endpoints := splitTrim(opt.Endpoints)
	if len(endpoints) == 0 {
		return NewInMemoryRegistry(nil), nil
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return NewEtcdRegistry(dialCtx, EtcdRegistryOptions{
		Endpoints: endpoints,
		Prefix:    opt.Prefix,
		Logger:    opt.Logger,
	})
}

func splitTrim(s string) []string {
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
