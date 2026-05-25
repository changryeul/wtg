package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/etcd/server/v3/embed"
)

// dev_etcd.go — DevMode 자동 기동 embedded etcd.
//
// 목적: `mci-admin --dev` 로 띄울 때 `--etcd` 가 비어있으면 in-process embed.Etcd
// 를 자동 기동해서 EtcdEndpoints 를 채워 넣는다. 결과적으로 symbols / pricing /
// profiles / user-profiles / quoteid-engines 같은 etcd 백 자원이 DevMode 에서도
// 코드 변경 없이 그대로 동작한다. (routes / policy 도 동일 etcd 사용.)
//
// 데이터 디렉터리는 안정적인 위치(`$WTG_DEV_ETCD_DIR` 또는
// `$XDG_DATA_HOME/wtg/dev-etcd` 또는 `~/.local/share/wtg/dev-etcd`) 를 써서
// 재시작 후에도 alias/symbols/pricing 편집이 보존된다.
//
// 운영용 아님 — `--etcd=<운영주소>` 가 명시되면 본 헬퍼는 호출되지 않는다.

// startDevEmbeddedEtcd 는 단일 노드 embed.Etcd 를 localhost random 포트로 띄우고
// ready 까지 대기한 후 (서버 핸들, clientURL) 을 반환.
//
// dataDir 가 비면 resolveDevEtcdDataDir 로 자동 결정.
func startDevEmbeddedEtcd(ctx context.Context, dataDir string, logger *slog.Logger) (*embed.Etcd, string, error) {
	if dataDir == "" {
		var err error
		dataDir, err = resolveDevEtcdDataDir()
		if err != nil {
			return nil, "", fmt.Errorf("dev etcd 데이터 디렉터리 결정: %w", err)
		}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("dev etcd 데이터 디렉터리 생성: %w", err)
	}

	cfg := embed.NewConfig()
	cfg.Dir = dataDir
	cfg.LogLevel = "error" // 부팅 noise 억제. 운영자에겐 mci-admin 의 로그만 노출.
	cfg.Name = "wtg-dev"

	// listen 주소 — 0 포트 (OS 가 free port 자동 선택).
	cliURL, _ := url.Parse("http://127.0.0.1:0")
	peerURL, _ := url.Parse("http://127.0.0.1:0")
	cfg.ListenClientUrls = []url.URL{*cliURL}
	cfg.AdvertiseClientUrls = []url.URL{*cliURL}
	cfg.ListenPeerUrls = []url.URL{*peerURL}
	cfg.AdvertisePeerUrls = []url.URL{*peerURL}
	cfg.InitialCluster = cfg.InitialClusterFromName(cfg.Name)

	srv, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("embed.StartEtcd: %w", err)
	}

	// ready 까지 대기. 15초 안에 안 뜨면 실패 처리.
	select {
	case <-srv.Server.ReadyNotify():
	case <-time.After(15 * time.Second):
		srv.Close()
		return nil, "", fmt.Errorf("dev etcd ready 타임아웃")
	case <-ctx.Done():
		srv.Close()
		return nil, "", ctx.Err()
	}

	if len(srv.Clients) == 0 {
		srv.Close()
		return nil, "", fmt.Errorf("dev etcd: client listener 없음")
	}
	clientURL := "http://" + srv.Clients[0].Addr().String()
	logger.Info("DevMode embedded etcd 활성",
		slog.String("client_url", clientURL),
		slog.String("data_dir", dataDir),
	)
	return srv, clientURL, nil
}

// resolveDevEtcdDataDir — 안정 데이터 디렉터리 결정.
// 우선순위:
//  1. $WTG_DEV_ETCD_DIR
//  2. $XDG_DATA_HOME/wtg/dev-etcd
//  3. $HOME/.local/share/wtg/dev-etcd
//  4. (fallback) $TMPDIR/wtg-dev-etcd — TMPDIR 가 OS 재부팅 후 삭제될 수 있으므로
//     경고 로깅은 호출자가 함.
func resolveDevEtcdDataDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("WTG_DEV_ETCD_DIR")); d != "" {
		return d, nil
	}
	if d := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); d != "" {
		return filepath.Join(d, "wtg", "dev-etcd"), nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "wtg", "dev-etcd"), nil
	}
	if tmp := strings.TrimSpace(os.Getenv("TMPDIR")); tmp != "" {
		return filepath.Join(tmp, "wtg-dev-etcd"), nil
	}
	return "", fmt.Errorf("dev etcd 데이터 디렉터리 결정 실패 — $HOME / $XDG_DATA_HOME / $TMPDIR 모두 미설정")
}
