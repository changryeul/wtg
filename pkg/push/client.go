// Package push — mci-push 의 HTTP push endpoint (POST /v1/internal/push) 호출
// Go 클라이언트 SDK. 운영 svc / admin tool 들이 import 해서 broker 우회 path
// 로 unsolicited 메시지를 발사할 때 사용.
//
// Phase-2: Phase-1 PoC (internal/push/http_push.go) 의 producer-side 동등 API.
//
// 사용 예:
//
//	cli := push.NewClient(push.ClientOptions{
//		BaseURL: "http://mci-push.internal:8081",
//		Secret:  os.Getenv("WTG_PUSH_SECRET"),
//		Timeout: 2 * time.Second,
//	})
//	defer cli.Close()
//
//	// user-targeted
//	if err := cli.Push(ctx, push.Message{User: "dealer01", Data: orderUpdate}); err != nil {
//		log.Warn("push 실패", err)
//	}
//
//	// broadcast (user 빈)
//	cli.Push(ctx, push.Message{Data: marketHalt})
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// Message — 발사할 push 한 건. 핸들러의 HTTPPushRequest 와 1:1 호환.
type Message struct {
	User string          `json:"user,omitempty"` // 비면 전체 broadcast
	Func uint8           `json:"func,omitempty"` // 0 = default (user 있으면 FCPush, 없으면 FCCast)
	Subc uint8           `json:"subc,omitempty"` // 0 = default (user 있으면 SubPush, 없으면 SubBroadcast)
	Data json.RawMessage `json:"data"`
}

// Result — 발사 결과. mci-push 의 HTTPPushResponse 와 동등.
type Result struct {
	Injected bool   `json:"injected"`
	Func     uint8  `json:"func"`
	Subc     uint8  `json:"subc"`
	User     string `json:"user,omitempty"`
	BodySize int    `json:"body_size"`
}

// Client — mci-push 의 HTTP push endpoint 호출 wrapper.
//
// thread-safe (http.Client 자체 thread-safe + 내부 상태 없음).
// 재사용 권장 — connection pool keep-alive.
type Client struct {
	baseURL string
	secret  string
	http    *http.Client
	initErr error // NewClient 의 TLS 구성 실패 — Push 시 반환.
}

// ClientOptions — Client 생성 의존성.
type ClientOptions struct {
	// BaseURL — mci-push 의 root URL. 예: "http://mci-push.internal:8081".
	// trailing "/" 자동 제거.
	BaseURL string

	// Secret — X-Push-Secret 헤더 값. mci-push 의 --push-secret 와 일치해야 함.
	// 빈값이면 헤더 미첨부 (mci-push 도 인증 disable 시에만 통과).
	Secret string

	// Timeout — HTTP 요청 timeout. 0 면 default 5s.
	Timeout time.Duration

	// HTTPClient — 직접 제공 시 사용 (사용자 정의 transport). nil 이면 default.
	// HTTPClient 채우면 TLS* 옵션은 무시 (사용자 책임).
	HTTPClient *http.Client

	// TLS — Phase 2.4 mTLS 옵션. ClientCertFile/KeyFile 채우면 client cert 첨부,
	// ServerCAFile 채우면 서버 인증서 검증 CA. 비면 시스템 trust store + HTTPS 면
	// 서버 cert 표준 검증.
	//
	// 운영 권장: 운영 svc 가 mci-push 와 mTLS 인증서 교환 (CN/SAN 기반 svc 식별).
	// 단일 secret 보다 인증서 폐기 / rotate 가 운영적으로 안전.
	TLSClientCertFile string
	TLSClientKeyFile  string
	TLSServerCAFile   string
	TLSServerName     string // SNI / hostname 검증. BaseURL 의 host 와 다르면 명시.
	TLSInsecure       bool   // 검증 skip — dev 자체발급용. **운영 금지**.
}

// NewClient — Client 생성. TLS 옵션 충돌 시 (cert 만 또는 key 만) panic 대신
// nil/error 가 아닌 silent fallback — 운영자가 set 했는데 동작 안 하면 안 되므로
// MustNewClient 와 분리.
//
// HTTPClient 가 nil 이고 TLS* 옵션이 있으면 자동으로 *http.Transport 구성.
func NewClient(opts ClientOptions) *Client {
	c, err := newClient(opts)
	if err != nil {
		// TLS 옵션 잘못된 경우 — fallback 으로 일반 client 만들고 다음 호출 시 fail 하도록.
		// 패키지 API 호환 위해 panic 대신 secret/baseURL 만 보존.
		return &Client{
			baseURL: strings.TrimRight(opts.BaseURL, "/"),
			secret:  opts.Secret,
			http:    &http.Client{Timeout: defaultTimeout(opts.Timeout)},
			initErr: err,
		}
	}
	return c
}

// MustNewClient — TLS 옵션 잘못 시 panic. 운영 svc 부팅 시 명시적으로 에러 노출.
func MustNewClient(opts ClientOptions) *Client {
	c, err := newClient(opts)
	if err != nil {
		panic(fmt.Sprintf("push.MustNewClient: %v", err))
	}
	return c
}

func newClient(opts ClientOptions) (*Client, error) {
	cli := opts.HTTPClient
	if cli == nil {
		// TLS 옵션 있으면 *http.Transport 자동 구성.
		var transport *http.Transport
		if opts.TLSClientCertFile != "" || opts.TLSServerCAFile != "" || opts.TLSInsecure {
			tlsCfg, err := tlsutil.LoadClient(tlsutil.ClientOptions{
				CertFile:           opts.TLSClientCertFile,
				KeyFile:            opts.TLSClientKeyFile,
				ServerCAFile:       opts.TLSServerCAFile,
				ServerName:         opts.TLSServerName,
				InsecureSkipVerify: opts.TLSInsecure,
			})
			if err != nil {
				return nil, fmt.Errorf("push: TLS 구성: %w", err)
			}
			transport = &http.Transport{TLSClientConfig: tlsCfg}
		}
		cli = &http.Client{Timeout: defaultTimeout(opts.Timeout)}
		if transport != nil {
			cli.Transport = transport
		}
	}
	return &Client{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		secret:  opts.Secret,
		http:    cli,
	}, nil
}

func defaultTimeout(t time.Duration) time.Duration {
	if t <= 0 {
		return 5 * time.Second
	}
	return t
}

// Push — 단건 발사. context cancel / timeout 시 immediate return.
// Result 가 nil 이라도 error 가 nil 이면 server 가 200 응답한 것.
func (c *Client) Push(ctx context.Context, msg Message) (*Result, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	if c.baseURL == "" {
		return nil, errors.New("push: BaseURL 미설정")
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("push: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/internal/push", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("push: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("X-Push-Secret", c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("push: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("push: HTTP %d: %s", resp.StatusCode, string(buf))
	}
	var out Result
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("push: decode: %w", err)
	}
	return &out, nil
}

// Close — http.Client idle connection 정리. 호출 안 해도 GC 가 처리하지만
// 명시적 shutdown 시 호출.
func (c *Client) Close() {
	if t, ok := c.http.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}
