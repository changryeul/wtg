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

	// HTTPClient — 직접 제공 시 사용 (mTLS / 사용자 정의 transport). nil 이면 default.
	HTTPClient *http.Client
}

// NewClient — Client 생성.
func NewClient(opts ClientOptions) *Client {
	cli := opts.HTTPClient
	if cli == nil {
		to := opts.Timeout
		if to <= 0 {
			to = 5 * time.Second
		}
		cli = &http.Client{Timeout: to}
	}
	return &Client{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		secret:  opts.Secret,
		http:    cli,
	}
}

// Push — 단건 발사. context cancel / timeout 시 immediate return.
// Result 가 nil 이라도 error 가 nil 이면 server 가 200 응답한 것.
func (c *Client) Push(ctx context.Context, msg Message) (*Result, error) {
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
