package krx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Start 는 HTTP/WS 서버를 가동한다 (블로킹). ctx 취소 시 graceful shutdown.
// 라우트: GET /v1/subscribe (ws), GET /healthz. --demo 면 합성 틱 소스도 가동.
func (srv *Server) Start(ctx context.Context, cfg Config) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/subscribe", srv.ServeWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok conns=%d\n", srv.hub.Count())
	})

	httpSrv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}

	if cfg.Demo {
		go srv.runDemoSource(ctx, cfg)
	}
	if cfg.Mcast {
		if err := srv.runMcastSource(ctx, cfg); err != nil {
			return err
		}
	}

	errCh := make(chan error, 1)
	go func() {
		srv.logger.Info("mci-edge-krx listen", slog.String("addr", cfg.ListenAddr),
			slog.Bool("demo", cfg.Demo))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

// runDemoSource 는 실 피드 배선 전 시연용으로 합성 KA(체결)/KB(호가) 틱을 주기 생성해
// Ingest 로 흘린다. 종목별로 값을 조금씩 흔들어 web 이 갱신을 볼 수 있게 한다.
func (srv *Server) runDemoSource(ctx context.Context, cfg Config) {
	codes := splitCodes(cfg.DemoCodes)
	if len(codes) == 0 {
		return
	}
	srv.logger.Info("데모 소스 가동", slog.Any("codes", codes),
		slog.Duration("interval", cfg.DemoInterval))
	t := time.NewTicker(cfg.DemoInterval)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for i, code := range codes {
				base := 200.0 + float64(i)*50.0
				last := base + float64(tick%20)*0.05
				if _, _, _, err := srv.IngestA306F(demoA306F(code, last)); err != nil {
					srv.logger.Warn("데모 A306F ingest", slog.Any("error", err))
				}
				if _, _, _, err := srv.IngestB606F(demoB606F(code, last)); err != nil {
					srv.logger.Warn("데모 B606F ingest", slog.Any("error", err))
				}
			}
			tick++
		}
	}
}

func splitCodes(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// demoA306F / demoB606F — 합성 원 TR (트랙2 파싱 경로 시연용 최소 필드).
// 오프셋은 pkg/krx a306f.go / b606f.go 와 동일 (A306F.h / B606F.h).
func demoA306F(code string, last float64) []byte {
	b := make([]byte, 173) // SZA306F
	for i := range b {
		b[i] = ' '
	}
	copy(b[0:5], "A306F")
	copy(b[17:29], code)
	copy(b[35:47], time.Now().Format("150405000")+"000")
	copy(b[47:56], fmt.Sprintf("%9.02f", last))        // cprc 체결가
	copy(b[83:92], fmt.Sprintf("%9.02f", last))        // oprc 시가
	copy(b[92:101], fmt.Sprintf("%9.02f", last+0.10))  // hprc 고가
	copy(b[101:110], fmt.Sprintf("%9.02f", last-0.10)) // lprc 저가
	copy(b[110:119], fmt.Sprintf("%9.02f", last))      // pprc 직전가
	copy(b[153:154], "2")                              // ftcd 매수
	return b
}

func demoB606F(code string, last float64) []byte {
	b := make([]byte, 324) // SZB606F
	for i := range b {
		b[i] = ' '
	}
	copy(b[0:5], "B606F")
	copy(b[17:29], code)
	copy(b[35:47], time.Now().Format("150405000")+"000")
	// hoga[0] @47 — sprc@0/bprc@9.
	copy(b[47:56], fmt.Sprintf("%9.02f", last+0.05)) // sell[0] 매도우선
	copy(b[56:65], fmt.Sprintf("%9.02f", last))      // buy[0]  매수우선
	return b
}
