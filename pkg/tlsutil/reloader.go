package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Reloader 는 운영 중 인증서를 atomic swap 으로 갱신할 수 있게 해 주는 holder.
//
// 사용:
//
//	rl, _ := NewReloader(opt)
//	tlsCfg := rl.ServerConfig()    // GetCertificate 가 atomic 으로 최신 cert 반환
//	rl.WatchSIGHUP(ctx)            // 운영자 SIGHUP → reload
//	rl.WatchFile(ctx, 30*time.Second) // mtime 변경 시 자동 reload
//
// cert-manager / Let's Encrypt 등 외부 도구가 cert 파일을 새로 쓰면 자동 적용.
type Reloader struct {
	certPath, keyPath string
	clientCAPath      string
	minVersion        uint16
	logger            *slog.Logger

	cert      atomic.Pointer[tls.Certificate]
	clientCAs atomic.Pointer[x509.CertPool]

	mu       sync.Mutex // Reload 직렬화
	lastMod  time.Time  // 파일 watcher 용

	stopOnce sync.Once
	stopC    chan struct{}
}

// ReloaderOptions — Reloader 생성 옵션.
type ReloaderOptions struct {
	CertFile     string // 필수
	KeyFile      string // 필수
	ClientCAFile string // 선택. mTLS 활성 시
	MinVersion   uint16 // 0 이면 TLS 1.2
	Logger       *slog.Logger
}

// NewReloader 는 1차 로딩을 수행한 뒤 Reloader 를 반환한다.
// 1차 로딩 실패 시 에러 — 시작도 못 함.
func NewReloader(opt ReloaderOptions) (*Reloader, error) {
	if opt.CertFile == "" || opt.KeyFile == "" {
		return nil, errors.New("tlsutil: CertFile/KeyFile 필수")
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	r := &Reloader{
		certPath:     opt.CertFile,
		keyPath:      opt.KeyFile,
		clientCAPath: opt.ClientCAFile,
		minVersion:   selectMinVersion(opt.MinVersion),
		logger:       opt.Logger,
		stopC:        make(chan struct{}),
	}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload 는 디스크에서 cert/key/ca 를 다시 읽어 atomic swap.
// 호출은 직렬화 (mu) — 동시 SIGHUP + 파일 watch 에서 안전.
func (r *Reloader) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return fmt.Errorf("tlsutil: cert/key 로딩: %w", err)
	}
	r.cert.Store(&cert)

	if r.clientCAPath != "" {
		pool, err := loadCABundle(r.clientCAPath)
		if err != nil {
			return fmt.Errorf("tlsutil: client CA 로딩: %w", err)
		}
		r.clientCAs.Store(pool)
	}
	if mt, err := mtimeOf(r.certPath); err == nil {
		r.lastMod = mt
	}
	r.logger.Info("tlsutil: 인증서 reload",
		slog.String("cert", r.certPath),
		slog.Bool("mtls", r.clientCAPath != ""),
	)
	return nil
}

// ServerConfig 는 GetCertificate / ClientCAs 가 atomic 하게 최신을 반환하는 *tls.Config.
//
// ClientCAFile 이 있으면 mTLS (tls.RequireAndVerifyClientCert).
// GetConfigForClient 로 클라이언트마다 최신 ClientCAs 를 반영 — CA 회전도 운영 중 가능.
func (r *Reloader) ServerConfig() *tls.Config {
	cfg := &tls.Config{
		MinVersion:     r.minVersion,
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := r.cert.Load()
			if c == nil {
				return nil, errors.New("tlsutil: 인증서 로딩 안됨")
			}
			return c, nil
		},
	}
	if r.clientCAPath != "" {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		// 매 클라이언트 요청마다 ClientCAs 반영 (atomic 갱신 시 다음 요청부터 적용).
		cfg.GetConfigForClient = func(_ *tls.ClientHelloInfo) (*tls.Config, error) {
			child := cfg.Clone()
			child.ClientCAs = r.clientCAs.Load()
			return child, nil
		}
	}
	return cfg
}

// WatchSIGHUP 는 SIGHUP 신호를 받아 Reload 를 호출하는 goroutine 을 띄운다.
// ctx 또는 Stop() 으로 종료.
func (r *Reloader) WatchSIGHUP() {
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-r.stopC:
				signal.Stop(sigC)
				return
			case <-sigC:
				if err := r.Reload(); err != nil {
					r.logger.Error("SIGHUP reload 실패", slog.Any("error", err))
				}
			}
		}
	}()
}

// WatchFile 은 cert 파일의 mtime 을 주기적으로 polling 하다 변경 시 Reload.
//
// fsnotify 의존성 추가를 피하고 단순 stat polling 사용. interval 은 보통
// 30s ~ 5min. 파일 시스템에 압박 없음.
func (r *Reloader) WatchFile(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-r.stopC:
				return
			case <-t.C:
				mt, err := mtimeOf(r.certPath)
				if err != nil {
					continue
				}
				r.mu.Lock()
				changed := mt.After(r.lastMod)
				r.mu.Unlock()
				if !changed {
					continue
				}
				if err := r.Reload(); err != nil {
					r.logger.Error("파일 watch reload 실패", slog.Any("error", err))
				}
			}
		}
	}()
}

// Stop — watcher goroutine 종료. idempotent.
func (r *Reloader) Stop() {
	r.stopOnce.Do(func() { close(r.stopC) })
}

// CurrentCertSubject 는 현재 cert 의 Subject CN — 디버그 / 모니터링용.
func (r *Reloader) CurrentCertSubject() string {
	c := r.cert.Load()
	if c == nil || len(c.Certificate) == 0 {
		return ""
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return ""
	}
	return leaf.Subject.CommonName
}

// mtimeOf — 파일 mtime helper.
func mtimeOf(path string) (time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}
