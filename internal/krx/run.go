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
				if _, _, _, err := srv.IngestKA(demoKA(code, last)); err != nil {
					srv.logger.Warn("데모 KA ingest", slog.Any("error", err))
				}
				if _, _, _, err := srv.IngestKB(demoKB(code, last)); err != nil {
					srv.logger.Warn("데모 KB ingest", slog.Any("error", err))
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

// demoKA / demoKB — 합성 전문 (테스트 buildKA/KB 와 동형, 시연용 최소 필드).
func demoKA(code string, last float64) []byte {
	b := make([]byte, 234)
	for i := range b {
		b[i] = ' '
	}
	copy(b[0:2], "KA")
	copy(b[2:14], code)
	copy(b[24:33], fmt.Sprintf("%-9.02f", last)) // oprc
	copy(b[34:43], fmt.Sprintf("%-9.02f", last)) // hprc
	copy(b[44:53], fmt.Sprintf("%-9.02f", last)) // lprc
	copy(b[54:63], fmt.Sprintf("%-9.02f", last)) // eprc last
	copy(b[107:108], "+")
	copy(b[108:120], time.Now().Format("150405000")+"000")
	return b
}

func demoKB(code string, last float64) []byte {
	b := make([]byte, 362)
	for i := range b {
		b[i] = ' '
	}
	copy(b[0:2], "KB")
	copy(b[2:14], code)
	copy(b[14:26], time.Now().Format("150405000")+"000")
	copy(b[122+1:122+10], fmt.Sprintf("%-9.02f", last+0.05)) // sell[0] prc
	copy(b[242+1:242+10], fmt.Sprintf("%-9.02f", last))      // buy[0] prc
	return b
}
