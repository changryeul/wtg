// Package tcp — mci-edge-tcp: 레거시 cs (HTS/EMP) 용 raw TCP 전문 gateway.
//
// 레거시 cs 클라이언트는 "지속 TCP 연결 + 고정폭 전문 + 주기 heartbeat" 를
// 말한다 — HTTPS 요청/응답인 mci-edge-api(8090) 에는 무수정으로 붙을 수 없다.
// 본 gateway 가 그 wire 를 그대로 받아 내부 mci-api 의 raw 전문 모드
// (POST /v1/tx, octet-stream) 로 변환한다. 클라이언트는 접속 좌표만 바꾸면 됨.
//
// Wire (mymq 컨벤션과 동일):
//   - frame  = [4B big-endian length][payload]
//   - length 0 (4B 모두 0) = heartbeat — 서버가 빈 프레임을 echo 해서
//     클라이언트가 왕복으로 생존을 확인한다.
//   - payload = COMHDR+Input 고정폭 전문. 앞 16B (COMHDR.trxc) 를 trim 해
//     alias 로 사용 → mci-api 가 라우팅 (패턴 rule 포함) 결정.
//   - 응답 frame = 엔진 output 전문 bytes 그대로 (비즈니스 에러 포함 —
//     레거시는 COMHDR rcod/mesg 로 판단). transport 에러는 text 본문이
//     그대로 frame 으로 나감 (Phase A — 전용 에러 전문은 Phase B).
//
// 요청은 connection 당 직렬 처리 (레거시 sync 전문 семantics). 다중
// 클라이언트는 connection 별 goroutine 으로 동시 처리.
//
// 인증 (Phase A): gateway 가 고정 자격으로 upstream 에 전달 —
// --api-user (DevMode X-WTG-User) 또는 --api-token (JWT Bearer).
// connection 별 LOGON 전문 인증은 Phase B.
package tcp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config — mci-edge-tcp 운영 옵션.
type Config struct {
	// ListenAddr — raw TCP listen 주소 (예: ":5021").
	ListenAddr string
	// UpstreamURL — 내부 mci-api base URL (예: http://127.0.0.1:8080).
	UpstreamURL string
	// APIUser — DevMode X-WTG-User 로 전달할 usid. APIToken 과 택일.
	APIUser string
	// APIToken — 운영 JWT (Authorization: Bearer). APIUser 보다 우선.
	APIToken string
	// Channel — X-WTG-Channel 값. 빈값이면 "HTS".
	Channel string
	// IdleTimeout — heartbeat 포함 무트래픽 허용 시간. 초과 시 연결 종료.
	// 0 이면 90s.
	IdleTimeout time.Duration
	// MaxFrame — 수신 frame payload 상한 (bytes). 0 이면 1MiB.
	MaxFrame int
	// UpstreamTimeout — /v1/tx 호출 timeout. 0 이면 10s.
	UpstreamTimeout time.Duration
}

func (c *Config) fill() {
	if c.Channel == "" {
		c.Channel = "HTS"
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 90 * time.Second
	}
	if c.MaxFrame == 0 {
		c.MaxFrame = 1 << 20
	}
	if c.UpstreamTimeout == 0 {
		c.UpstreamTimeout = 10 * time.Second
	}
}

// Server — raw TCP 전문 gateway.
type Server struct {
	cfg    Config
	logger *slog.Logger
	httpc  *http.Client

	mu    sync.Mutex
	ln    net.Listener
	conns int64 // 누적 연결 수 (진단 로그용)
}

// NewServer — cfg 검증 + 기본값 채움.
func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	cfg.fill()
	if cfg.ListenAddr == "" {
		return nil, errors.New("tcp: ListenAddr 필수")
	}
	if cfg.UpstreamURL == "" {
		return nil, errors.New("tcp: UpstreamURL 필수 (내부 mci-api)")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:    cfg,
		logger: logger,
		httpc:  &http.Client{Timeout: cfg.UpstreamTimeout},
	}, nil
}

// Addr — listen 중인 실제 주소 (테스트에서 :0 사용 시).
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Start — listen + accept 루프 (블로킹). ctx 취소 시 반환.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("tcp listen: %w", err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.logger.Info("mci-edge-tcp listen 시작",
		slog.String("addr", ln.Addr().String()),
		slog.String("upstream", s.cfg.UpstreamURL),
		slog.String("channel", s.cfg.Channel),
		slog.Duration("idle_timeout", s.cfg.IdleTimeout),
	)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // 정상 종료
			}
			return fmt.Errorf("tcp accept: %w", err)
		}
		s.mu.Lock()
		s.conns++
		id := s.conns
		s.mu.Unlock()
		go s.handleConn(ctx, conn, id)
	}
}

// handleConn — connection 당 직렬 처리 루프.
func (s *Server) handleConn(ctx context.Context, conn net.Conn, id int64) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	s.logger.Info("tcp 연결", slog.Int64("conn", id), slog.String("remote", remote))
	defer s.logger.Info("tcp 종료", slog.Int64("conn", id), slog.String("remote", remote))

	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		payload, err := readFrame(conn, s.cfg.MaxFrame)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.logger.Warn("tcp frame 수신 실패 — 연결 종료",
					slog.Int64("conn", id), slog.Any("error", err))
			}
			return
		}
		// heartbeat — 빈 frame echo (클라이언트 왕복 생존 확인).
		if len(payload) == 0 {
			if err := writeFrame(conn, nil); err != nil {
				return
			}
			continue
		}
		resp, err := s.forward(ctx, payload)
		if err != nil {
			s.logger.Warn("upstream forward 실패",
				slog.Int64("conn", id), slog.Any("error", err))
			// transport 실패도 클라이언트에 알림 — text 본문 frame (Phase A).
			resp = []byte("WTG-EDGE-TCP-ERROR: " + err.Error())
		}
		if err := writeFrame(conn, resp); err != nil {
			return
		}
	}
}

// forward — 전문 payload 를 mci-api raw 모드로 전달.
// 앞 16B (COMHDR.trxc) 를 alias 로 사용. HTTP status 무관하게 응답 body 를
// 반환 — 비즈니스 에러는 output 전문 (200), transport 에러는 text.
func (s *Server) forward(ctx context.Context, payload []byte) ([]byte, error) {
	if len(payload) < 16 {
		return nil, fmt.Errorf("전문 길이 부족: %dB (< COMHDR.trxc 16B)", len(payload))
	}
	trxc := strings.TrimSpace(string(payload[:16]))
	if trxc == "" {
		return nil, errors.New("COMHDR.trxc 비어있음")
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.UpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		strings.TrimRight(s.cfg.UpstreamURL, "/")+"/v1/tx", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-WTG-Alias", trxc)
	req.Header.Set("X-WTG-Channel", s.cfg.Channel)
	if s.cfg.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.APIToken)
	} else if s.cfg.APIUser != "" {
		req.Header.Set("X-WTG-User", s.cfg.APIUser)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(s.cfg.MaxFrame)))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// ───────── framing — [4B big-endian length][payload], length 0 = heartbeat ─────────

func readFrame(r io.Reader, maxFrame int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, nil // heartbeat
	}
	if int(n) > maxFrame {
		return nil, fmt.Errorf("frame %dB > 상한 %dB", n, maxFrame)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeFrame(w io.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}
