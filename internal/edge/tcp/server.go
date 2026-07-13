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
	"encoding/json"
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
	// StatsAddr — 진단 HTTP listen 주소 (예: "127.0.0.1:5022"). 빈값 = 비활성.
	// GET /stats (연결/카운터 + 연결별 상세, CORS *) + GET /healthz.
	// admin 의 /v1/admin/tcp-gw/stats proxy 와 대시보드 mci-health 가 소비.
	StatsAddr string
	// SelectServerIP — cs 의 select-server (FC 0x01) 응답에 넣을 서버 IP.
	// cs 는 ip1==ip2 면 같은 소켓 유지, 다르면 ip2 로 재접속(LB). 두 필드에
	// 동일 값을 넣어 재접속 없이 진행시킨다. 빈값이면 conn 의 LocalAddr IP 사용.
	SelectServerIP string
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

	mu      sync.Mutex
	ln      net.Listener
	statsLn net.Listener
	conns   int64 // 누적 연결 수 (id 발급 겸용)
	active  map[int64]*connInfo

	framesIn       int64 // 전문 frame 수신 (heartbeat 제외)
	framesOut      int64
	heartbeats     int64
	upstreamErrors int64
}

// connInfo — 활성 연결 1건의 진단 정보.
type connInfo struct {
	ID          int64     `json:"id"`
	Remote      string    `json:"remote"`
	ConnectedAt time.Time `json:"connected_at"`
	// 아래 필드는 s.mu 로 보호.
	LastActivity time.Time `json:"last_activity"`
	Frames       int64     `json:"frames"`
	Heartbeats   int64     `json:"heartbeats"`
	// Usid — 마지막 전문 COMHDR 의 usid (관측용). HTS 는 연결당 1사용자라
	// 사실상 연결 주인. connection 별 인증 (LOGON) 은 Phase B — 이 값은
	// 클라이언트 신고값이지 검증된 신원이 아님에 유의.
	Usid string `json:"usid,omitempty"`
}

// COMHDR 의 usid 위치 — win/src/inc/com/comhdr.h 필드 배치
// (trxc[16] scrn[6] loip[16] auip[16] maca[20] → usid[30]).
const (
	comhdrUsidOff = 74
	comhdrUsidLen = 30
)

// cs 프로토콜 FunctionCode (NymphSocket.cpp FunctionCode 표).
//
//	0x01 select-server / 0x02 crypto 키교환 / 'A' sign-on / 'B' cookie / 'C' 전문.
const (
	fcSelectServer = 0x01
	fcCrypto       = 0x02
	fcSignOn       = 'A' // 0x41
	fcCookie       = 'B' // 0x42 — 로그온 쿠키 (응답 불필요)
	fcTx           = 'C' // 0x43 — 일반 전문
	// select-server 응답의 ip 필드 크기 (ASCII null-terminated).
	selectServerIPLen = 24
	// TH(3B) flags 비트 (NymphSocket.h). RHI set 이면 RH(37B) 첨부.
	thFlagRHI = 0x01
	thLen     = 3
	rhLen     = 37
	// cs TH-framed 프레임 판별 — payload[0] 가 flags(<0x20 제어값) 면 TH 프레임,
	// ASCII trxc(영문, ≥0x20) 면 raw COMHDR (tcp-tester 등 TH 없는 경로).
	thFlagsMax = 0x20
)

// replySelectServer 는 cs 의 select-server (FC 0x01) 요청에 응답한다.
// 응답 = [TH echo 3B][ip1 24B][ip2 24B]. cs 는 strcmp(ip1,ip2) 만 보고
// 같으면 현 소켓 유지, 다르면 ip2 로 재접속(LB). 두 필드에 동일 IP 를 넣어
// 재접속 없이 진행시킨다. IP 는 cfg.SelectServerIP, 빈값이면 conn LocalAddr.
func (s *Server) replySelectServer(conn net.Conn, reqTH []byte) error {
	ip := s.cfg.SelectServerIP
	if ip == "" {
		if host, _, err := net.SplitHostPort(conn.LocalAddr().String()); err == nil {
			ip = host
		}
	}
	ipField := make([]byte, selectServerIPLen) // null-padded ASCII
	copy(ipField, ip)

	resp := make([]byte, 0, 3+2*selectServerIPLen)
	resp = append(resp, reqTH[:3]...) // TH echo (0c 00 01)
	resp = append(resp, ipField...)   // ip1
	resp = append(resp, ipField...)   // ip2 (== ip1)

	s.logger.Info("select-server 응답", slog.String("ip", ip))
	return writeFrame(conn, resp)
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
		active: make(map[int64]*connInfo),
	}, nil
}

// StatsAddr — stats HTTP 의 실제 주소 (미활성이면 nil).
func (s *Server) StatsAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statsLn == nil {
		return nil
	}
	return s.statsLn.Addr()
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
	if err := s.startStats(ctx); err != nil {
		_ = ln.Close()
		return err
	}
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
		ci := &connInfo{
			ID: s.conns, Remote: conn.RemoteAddr().String(),
			ConnectedAt: time.Now(), LastActivity: time.Now(),
		}
		s.active[ci.ID] = ci
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.active, ci.ID)
				s.mu.Unlock()
			}()
			s.handleConn(ctx, conn, ci)
		}()
	}
}

// startStats — 진단 HTTP listener (옵션). CORS * — fix/md stats 와 동일 컨벤션.
func (s *Server) startStats(ctx context.Context) error {
	if s.cfg.StatsAddr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.StatsAddr)
	if err != nil {
		return fmt.Errorf("stats listen: %w", err)
	}
	s.mu.Lock()
	s.statsLn = ln
	s.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		s.mu.Lock()
		conns := make([]connInfo, 0, len(s.active))
		for _, ci := range s.active {
			conns = append(conns, *ci)
		}
		out := map[string]any{
			"forward_user":    s.cfg.APIUser, // gateway 가 upstream 에 쓰는 고정 인증 주체
			"channel":         s.cfg.Channel,
			"active_conns":    int64(len(s.active)),
			"total_conns":     s.conns,
			"frames_in":       s.framesIn,
			"frames_out":      s.framesOut,
			"heartbeats":      s.heartbeats,
			"upstream_errors": s.upstreamErrors,
			"conns":           conns,
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() { _ = srv.Serve(ln) }()
	s.logger.Info("stats HTTP 시작", slog.String("addr", ln.Addr().String()))
	return nil
}

// handleConn — connection 당 직렬 처리 루프.
func (s *Server) handleConn(ctx context.Context, conn net.Conn, ci *connInfo) {
	defer conn.Close()
	s.logger.Info("tcp 연결", slog.Int64("conn", ci.ID), slog.String("remote", ci.Remote))
	defer s.logger.Info("tcp 종료", slog.Int64("conn", ci.ID), slog.String("remote", ci.Remote))

	touch := func(hb bool) {
		s.mu.Lock()
		ci.LastActivity = time.Now()
		if hb {
			ci.Heartbeats++
			s.heartbeats++
		} else {
			ci.Frames++
			s.framesIn++
		}
		s.mu.Unlock()
	}

	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		payload, err := readFrame(conn, s.cfg.MaxFrame)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.logger.Warn("tcp frame 수신 실패 — 연결 종료",
					slog.Int64("conn", ci.ID), slog.Any("error", err))
			}
			return
		}
		// heartbeat — 빈 frame echo (클라이언트 왕복 생존 확인).
		if len(payload) == 0 {
			touch(true)
			if err := writeFrame(conn, nil); err != nil {
				return
			}
			continue
		}
		// cs 프로토콜 제어 프레임 — payload 앞 3B 가 TH(transport header):
		//   TH[0]=BPI+EPI flags(0x0c=단일완결), TH[2]=FunctionCode.
		//   FC 0x01 = select-server (LB 라우팅) — 연결 직후 무조건 최초.
		//   전문(sign-on 'A'/cookie 'B'/일반 'C') 전에 반드시 통과해야 함.
		// 자세히는 docs/edge-tcp-cs-protocol.md.
		if len(payload) == 3 && payload[2] == fcSelectServer {
			touch(false)
			if err := s.replySelectServer(conn, payload); err != nil {
				return
			}
			continue
		}
		// crypto 키교환 (FC 0x02) — HTS 모드(m_bHTSYN) INISAFENET 세션키 교환.
		// 미구현: cs 를 HTSYN=N 으로 설정하면 이 단계 생략 (docs/edge-tcp-cs-protocol.md).
		if len(payload) >= 3 && payload[2] == fcCrypto {
			touch(false)
			s.logger.Warn("crypto(FC 0x02) 미구현 — cs [Application] HTSYN=N 설정 필요",
				slog.Int64("conn", ci.ID), slog.Int("payload_len", len(payload)))
			continue
		}
		touch(false)

		// 프레이밍 판별:
		//   cs TH-framed — payload[0] 가 flags(<0x20 제어값). [TH 3B][RH 37B?][COMHDR body]
		//   raw COMHDR   — payload[0] 가 ASCII trxc(≥0x20). tcp-tester 등 TH 없는 경로.
		header, body := []byte(nil), payload
		if len(payload) >= thLen && payload[0] < thFlagsMax {
			hdrLen := thLen
			if payload[0]&thFlagRHI != 0 {
				hdrLen += rhLen // RHI set → RH(37B) 첨부
			}
			if len(payload) < hdrLen {
				s.logger.Warn("TH/RH 헤더 길이 부족", slog.Int64("conn", ci.ID), slog.Int("len", len(payload)))
				continue
			}
			header, body = payload[:hdrLen], payload[hdrLen:]
			// cookie(FC 'B') — 서버는 신원 accept 만, 응답 없음 (cs 가 안 기다림).
			if payload[2] == fcCookie {
				s.captureUsid(ci, body)
				s.logger.Info("cs cookie(FC B) 수신 — 신원 등록", slog.Int64("conn", ci.ID), slog.String("usid", ci.Usid))
				continue
			}
		}

		s.captureUsid(ci, body)
		resp, err := s.forward(ctx, body)
		if err != nil {
			s.logger.Warn("upstream forward 실패",
				slog.Int64("conn", ci.ID), slog.Any("error", err))
			s.mu.Lock()
			s.upstreamErrors++
			s.mu.Unlock()
			resp = []byte("WTG-EDGE-TCP-ERROR: " + err.Error())
		}
		// cs TH-framed 요청은 응답도 [TH][RH] echo 로 재프레이밍 (cs recv 파서가
		// RH.RoutingKey 로 요청-응답 매칭). raw COMHDR 는 그대로 (tcp-tester).
		out := resp
		if header != nil {
			out = append(append([]byte(nil), header...), resp...)
		}
		if err := writeFrame(conn, out); err != nil {
			return
		}
		s.mu.Lock()
		s.framesOut++
		s.mu.Unlock()
	}
}

// captureUsid — 전문 COMHDR body 의 usid 를 연결에 기록 (관측용).
func (s *Server) captureUsid(ci *connInfo, body []byte) {
	if len(body) >= comhdrUsidOff+comhdrUsidLen {
		if u := strings.TrimSpace(string(body[comhdrUsidOff : comhdrUsidOff+comhdrUsidLen])); u != "" {
			s.mu.Lock()
			ci.Usid = u
			s.mu.Unlock()
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
