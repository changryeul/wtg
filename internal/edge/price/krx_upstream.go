package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	"github.com/winwaysystems/wtg/pkg/instrument"
	"github.com/winwaysystems/wtg/pkg/lpcatalog"
	"github.com/winwaysystems/wtg/pkg/policy"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// KRX 상류 fan-in + 통합 카탈로그 — Phase 2. mci-price-krx 의 KrxPriceService 에서
// KRX 이벤트를 받아 폴리모픽 v2 envelope 로 종목 구독자에게 fan-out 한다.
// docs/unified-quote-edge-design.md §5·§6.

// startCatalog — 통합 Instrument 카탈로그 로드. etcd(prefix) 우선, 없으면 파일.
// 둘 다 비면 no-op (카탈로그 비활성 = FX 전용 기존 동작).
func (s *Server) startCatalog(ctx context.Context) error {
	// 이미 주입(테스트/명시)된 경우 존중.
	if s.catalog != nil {
		return nil
	}
	// etcd 모드 — EtcdEndpoints + InstrumentsEtcdPrefix.
	if s.cfg.EtcdEndpoints != "" && s.cfg.InstrumentsEtcdPrefix != "" {
		eps := policy.SplitEndpoints(s.cfg.EtcdEndpoints)
		if len(eps) > 0 {
			cli, err := clientv3.New(clientv3.Config{Endpoints: eps, DialTimeout: 5 * time.Second})
			if err != nil {
				return fmt.Errorf("catalog etcd dial: %w", err)
			}
			cat := instrument.NewCatalog()
			w, err := instrument.NewEtcdCatalogWatcher(ctx, instrument.EtcdCatalogWatcherOptions{
				Client: cli, Prefix: s.cfg.InstrumentsEtcdPrefix, C: cat, Logger: s.logger,
			})
			if err != nil {
				_ = cli.Close()
				return fmt.Errorf("catalog watcher: %w", err)
			}
			s.catalog = cat
			s.catalogWatcher = w
			s.catalogEtcdCli = cli
			s.logger.Info("Instrument 카탈로그 etcd watcher 활성",
				slog.String("prefix", s.cfg.InstrumentsEtcdPrefix), slog.Int("count", cat.Size()))
			return nil
		}
	}
	// 파일 모드.
	if s.cfg.InstrumentsFile != "" {
		items, err := instrument.LoadFile(s.cfg.InstrumentsFile)
		if err != nil {
			return err
		}
		cat := instrument.NewCatalog()
		cat.Replace(items)
		s.catalog = cat
		s.logger.Info("Instrument 카탈로그 파일 로드",
			slog.String("file", s.cfg.InstrumentsFile), slog.Int("count", cat.Size()))
	}
	return nil
}

// startLPCatalog — FX LP 카탈로그 로드 (client /v1/sources 노출용). etcd(prefix)
// 우선, 없으면 파일. 둘 다 비면 no-op (/v1/sources 빈 목록). mci-price 와 동일 config.
func (s *Server) startLPCatalog(ctx context.Context) error {
	if s.lpCat != nil { // 이미 주입(테스트) 시 존중
		return nil
	}
	// etcd 모드.
	if s.cfg.EtcdEndpoints != "" && s.cfg.LPEtcdPrefix != "" {
		eps := policy.SplitEndpoints(s.cfg.EtcdEndpoints)
		if len(eps) > 0 {
			cli, err := clientv3.New(clientv3.Config{Endpoints: eps, DialTimeout: 5 * time.Second})
			if err != nil {
				return fmt.Errorf("lp catalog etcd dial: %w", err)
			}
			cat := lpcatalog.NewCatalog()
			w, err := lpcatalog.NewEtcdWatcher(ctx, lpcatalog.EtcdWatcherOptions{
				Client: cli, Prefix: s.cfg.LPEtcdPrefix, C: cat, Logger: s.logger,
			})
			if err != nil {
				_ = cli.Close()
				return fmt.Errorf("lp catalog watcher: %w", err)
			}
			s.lpCat = cat
			s.lpWatcher = w
			s.lpEtcdCli = cli
			s.logger.Info("LP 카탈로그 etcd watcher 활성 (/v1/sources)",
				slog.String("prefix", s.cfg.LPEtcdPrefix), slog.Int("count", cat.Size()))
			return nil
		}
	}
	// 파일 모드.
	if s.cfg.LPFile != "" {
		items, err := lpcatalog.LoadFile(s.cfg.LPFile)
		if err != nil {
			return err
		}
		cat := lpcatalog.NewCatalog()
		cat.Replace(items)
		s.lpCat = cat
		s.logger.Info("LP 카탈로그 파일 로드 (/v1/sources)",
			slog.String("file", s.cfg.LPFile), slog.Int("count", cat.Size()))
	}
	return nil
}

// startKrxUpstream — mci-price-krx KrxPriceService 로 dial + consume 루프 가동.
func (s *Server) startKrxUpstream(ctx context.Context) error {
	creds, err := s.upstreamCreds()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(s.cfg.KrxUpstreamGRPC, grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("KRX gRPC NewClient %s: %w", s.cfg.KrxUpstreamGRPC, err)
	}
	s.krxUpstream = conn
	go s.subscribeKrxLoop(ctx)
	s.logger.Info("KRX 상류 fan-in 활성", slog.String("upstream", s.cfg.KrxUpstreamGRPC))
	return nil
}

// subscribeKrxLoop — SubscribeKrx stream 유지 + 끊김 시 exponential backoff 재연결.
func (s *Server) subscribeKrxLoop(ctx context.Context) {
	client := wtgpb.NewKrxPriceServiceClient(s.krxUpstream)
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.consumeKrxOnce(ctx, client)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		s.logger.Warn("SubscribeKrx stream 끊김 — 재시도",
			slog.Any("error", err), slog.Duration("backoff", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}
}

// consumeKrxOnce — 단일 SubscribeKrx stream lifecycle. 전체 종목 구독(빈 필터),
// 종목 필터링은 edge 의 ws subscriber 단에서 (MatchesPair).
func (s *Server) consumeKrxOnce(ctx context.Context, client wtgpb.KrxPriceServiceClient) error {
	stream, err := client.SubscribeKrx(ctx, &wtgpb.KrxSubscribeRequest{SubscriberId: s.cfg.SubscriberID})
	if err != nil {
		return err
	}
	s.logger.Info("SubscribeKrx 시작", slog.String("subscriber_id", s.cfg.SubscriberID))
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return errors.New("upstream KRX EOF")
		}
		if err != nil {
			return err
		}
		s.totalKrxRecv.Add(1)
		v1, v2 := encodeKrxVariants(ev)
		// KRX 는 profile/마진 무관 — 종목(symbol) 매칭 구독자에게 fan-out.
		s.registry.BroadcastBySymbolV(ev.GetSymbol(), v1, v2)
	}
}

// encodeKrxVariants — KrxEvent → legacy(v1: 원 struct JSON) + v2(폴리모픽 envelope).
// v1 은 payload 그대로(기존 KRX flat), v2 는 통합 헤더로 wrap.
func encodeKrxVariants(ev *wtgpb.KrxEvent) (v1, v2 []byte) {
	v1 = ev.GetPayload() // 원 struct JSON (legacy flat {kind,code,...})
	env := struct {
		EV         int             `json:"ev"`
		Type       string          `json:"type"`
		AssetClass string          `json:"asset_class"`
		Symbol     string          `json:"symbol"`
		TSUnixNano int64           `json:"ts_unix_nano"`
		Data       json.RawMessage `json:"data"`
	}{
		EV:         2,
		Type:       ev.GetType(),
		AssetClass: ev.GetAssetClass(),
		Symbol:     ev.GetSymbol(),
		TSUnixNano: ev.GetTsUnixNano(),
		Data:       json.RawMessage(ev.GetPayload()),
	}
	if b, err := json.Marshal(env); err == nil {
		v2 = b
	}
	return v1, v2
}
