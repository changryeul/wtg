package chart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// subscribeBarLoop 은 mci-price 의 PriceService.SubscribeBar 를 호출하고
// 끊기면 backoff 재시도. 모든 봉을 Hub 로 fan-out.
//
// UpstreamGRPC 가 비어있으면 호출되지 않음 (Server.Start 가 분기).
func (s *Server) subscribeBarLoop(ctx context.Context) {
	creds, err := s.upstreamCreds()
	if err != nil {
		s.logger.Error("Upstream gRPC TLS 구성 실패", slog.Any("error", err))
		return
	}
	conn, err := grpc.NewClient(s.cfg.UpstreamGRPC, grpc.WithTransportCredentials(creds))
	if err != nil {
		s.logger.Error("Upstream gRPC dial 실패", slog.Any("error", err))
		return
	}
	defer conn.Close()
	s.logger.Info("Upstream gRPC 연결",
		slog.String("addr", s.cfg.UpstreamGRPC),
		slog.Bool("tls", s.cfg.GRPCTLSCertFile != "" || s.cfg.GRPCTLSCAFile != ""),
	)

	client := wtgpb.NewPriceServiceClient(conn)
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.consumeBarOnce(ctx, client)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		s.logger.Warn("SubscribeBar stream 끊김 — 재시도",
			slog.Any("error", err),
			slog.Duration("backoff", backoff),
		)
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

// consumeBarOnce 는 단일 SubscribeBar stream 의 lifecycle.
func (s *Server) consumeBarOnce(ctx context.Context, client wtgpb.PriceServiceClient) error {
	req := &wtgpb.BarSubscribeRequest{
		SubscriberId: s.cfg.SubscriberID,
		// pair/tf 필터 비움 — 모든 봉을 받아 ws 단계에서 (pair, tf) 매칭.
	}
	stream, err := client.SubscribeBar(ctx, req)
	if err != nil {
		return err
	}
	s.logger.Info("SubscribeBar 시작", slog.String("subscriber_id", s.cfg.SubscriberID))

	for {
		bar, err := stream.Recv()
		if err == io.EOF {
			return errors.New("upstream bar EOF")
		}
		if err != nil {
			return err
		}
		s.totalBarsRecv.Add(1)
		payload, err := encodeBarJSON(bar)
		if err != nil {
			s.logger.Warn("bar JSON 직렬화 실패", slog.Any("error", err))
			continue
		}
		s.hub.Publish(bar.GetPair(), bar.GetTf(), payload)
	}
}

// upstreamCreds — Internal mci-price 호출용 gRPC TransportCredentials.
// 인증서 있으면 mTLS, 없으면 insecure (dev/dev-cluster).
func (s *Server) upstreamCreds() (credentials.TransportCredentials, error) {
	if s.cfg.GRPCTLSCertFile == "" && s.cfg.GRPCTLSCAFile == "" {
		return insecure.NewCredentials(), nil
	}
	tlsCfg, err := tlsutil.LoadClient(tlsutil.ClientOptions{
		CertFile:     s.cfg.GRPCTLSCertFile,
		KeyFile:      s.cfg.GRPCTLSKeyFile,
		ServerCAFile: s.cfg.GRPCTLSCAFile,
		ServerName:   s.cfg.GRPCTLSServerName,
	})
	if err != nil {
		return nil, fmt.Errorf("upstream TLS: %w", err)
	}
	return credentials.NewTLS(tlsCfg), nil
}

// encodeBarJSONFromQuote — quote.Bar (DB Repository.QueryBars 결과) → ws envelope.
// encodeBarJSON 의 wtgpb.Bar 변형과 동일 모양 + type="bar" — 클라이언트가
// live / backfill 차이 없이 처리 가능.
func encodeBarJSONFromQuote(b quote.Bar) ([]byte, error) {
	out := struct {
		Type      string  `json:"type"`
		Pair      string  `json:"pair"`
		TF        string  `json:"tf"`
		OpenedAt  string  `json:"opened_at"`
		ClosedAt  string  `json:"closed_at"`
		OpenBid   float64 `json:"open_bid"`
		OpenAsk   float64 `json:"open_ask"`
		HighBid   float64 `json:"high_bid"`
		HighAsk   float64 `json:"high_ask"`
		LowBid    float64 `json:"low_bid"`
		LowAsk    float64 `json:"low_ask"`
		CloseBid  float64 `json:"close_bid"`
		CloseAsk  float64 `json:"close_ask"`
		TickCount int     `json:"tick_count"`
		// Source — "backfill" 만 표시 (live 는 omit). 클라이언트가 backfill 인지
		// 인식해서 progress bar / loading 등 표시 가능.
		Source string `json:"source,omitempty"`
	}{
		Type:      "bar",
		Pair:      string(b.Pair),
		TF:        string(b.TF),
		OpenedAt:  b.OpenedAt.UTC().Format(time.RFC3339Nano),
		ClosedAt:  b.ClosedAt.UTC().Format(time.RFC3339Nano),
		OpenBid:   b.OpenBid,
		OpenAsk:   b.OpenAsk,
		HighBid:   b.HighBid,
		HighAsk:   b.HighAsk,
		LowBid:    b.LowBid,
		LowAsk:    b.LowAsk,
		CloseBid:  b.CloseBid,
		CloseAsk:  b.CloseAsk,
		TickCount: b.TickCount,
		Source:    "backfill",
	}
	return json.Marshal(out)
}

// encodeBarJSON 은 proto Bar → 클라이언트 JSON envelope.
//
// BarDTO 와 호환되는 모양 — REST 응답과 같은 필드명. type:"bar" 가 추가.
func encodeBarJSON(b *wtgpb.Bar) ([]byte, error) {
	out := struct {
		Type      string  `json:"type"`
		Pair      string  `json:"pair"`
		TF        string  `json:"tf"`
		OpenedAt  string  `json:"opened_at"`
		ClosedAt  string  `json:"closed_at"`
		OpenBid   float64 `json:"open_bid"`
		OpenAsk   float64 `json:"open_ask"`
		HighBid   float64 `json:"high_bid"`
		HighAsk   float64 `json:"high_ask"`
		LowBid    float64 `json:"low_bid"`
		LowAsk    float64 `json:"low_ask"`
		CloseBid  float64 `json:"close_bid"`
		CloseAsk  float64 `json:"close_ask"`
		TickCount int     `json:"tick_count"`
	}{
		Type:      "bar",
		Pair:      b.GetPair(),
		TF:        b.GetTf(),
		OpenedAt:  time.Unix(0, b.GetOpenedUnixNano()).UTC().Format(time.RFC3339Nano),
		ClosedAt:  time.Unix(0, b.GetClosedUnixNano()).UTC().Format(time.RFC3339Nano),
		OpenBid:   b.GetOpenBid(),
		OpenAsk:   b.GetOpenAsk(),
		HighBid:   b.GetHighBid(),
		HighAsk:   b.GetHighAsk(),
		LowBid:    b.GetLowBid(),
		LowAsk:    b.GetLowAsk(),
		CloseBid:  b.GetCloseBid(),
		CloseAsk:  b.GetCloseAsk(),
		TickCount: int(b.GetTickCount()),
	}
	return json.Marshal(out)
}
