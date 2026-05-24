//go:build integration

// Package etcdtest 는 통합 테스트용 embedded etcd 헬퍼.
//
// 활성화: `go test -tags=integration ./...` (의존성 격리).
// 운영 바이너리에는 들어가지 않음 — _test.go 또는 build tag 로 import.
package etcdtest

import (
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/etcd/server/v3/embed"
)

// Embedded 는 띄운 embedded etcd 의 핸들 + 클라이언트 endpoint.
type Embedded struct {
	Server    *embed.Etcd
	ClientURL string // "http://127.0.0.1:NNNN"
}

// Start 는 임시 디렉토리에 embedded etcd 를 띄우고 첫 ready 까지 대기.
//
// 단일 노드 (no peer), 로컬 random 포트. t.Cleanup 으로 자동 정리.
func Start(t *testing.T) *Embedded {
	t.Helper()
	dir := t.TempDir()

	cfg := embed.NewConfig()
	cfg.Dir = filepath.Join(dir, "etcd")
	cfg.LogLevel = "error"

	// listen 주소 — 0 포트 (OS 가 free port 선택).
	cliURL := mustURL(t, "http://127.0.0.1:0")
	peerURL := mustURL(t, "http://127.0.0.1:0")
	cfg.ListenClientUrls = []url.URL{*cliURL}
	cfg.AdvertiseClientUrls = []url.URL{*cliURL}
	cfg.ListenPeerUrls = []url.URL{*peerURL}
	cfg.AdvertisePeerUrls = []url.URL{*peerURL}
	cfg.InitialCluster = cfg.InitialClusterFromName(cfg.Name)

	srv, err := embed.StartEtcd(cfg)
	if err != nil {
		t.Fatalf("embedded etcd 시작: %v", err)
	}

	select {
	case <-srv.Server.ReadyNotify():
	case <-time.After(15 * time.Second):
		srv.Close()
		t.Fatal("embedded etcd ready 타임아웃")
	}

	// 실제 bind 된 endpoint.
	clients := srv.Clients
	if len(clients) == 0 {
		srv.Close()
		t.Fatal("embedded etcd: client listener 없음")
	}
	clientURL := "http://" + clients[0].Addr().String()

	t.Cleanup(func() {
		srv.Close()
	})

	return &Embedded{
		Server:    srv,
		ClientURL: clientURL,
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL parse %q: %v", raw, err)
	}
	return u
}
