package admin

import (
	"encoding/json"
	"net/http"
)

// BrokerConnStatus — broker connection 상태 응답. UI 가 페이지 진입 시 1회
// fetch 해서 banner 표시 (broker 명령 / Tx 테스터 / Push 테스터 / WS 모니터
// 등 broker 의존 페이지).
type BrokerConnStatus struct {
	// Connected — broker 와 통신 가능 (실제 RPC 가 도달할 수 있음).
	Connected bool `json:"connected"`
	// Mode — "ok" / "no_broker" / "reconnecting" / "closed".
	Mode string `json:"mode"`
	// Message — 운영자/사용자 친화 한글 메시지.
	Message string `json:"message"`
	// Host/Port — broker 위치 (no_broker 모드는 비움).
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Appl — admin 의 ApplName (broker 측 whois 용).
	Appl string `json:"appl,omitempty"`
}

// BrokerConn — GET /v1/admin/broker-conn. mci-admin 의 broker connection 상태.
func BrokerConn(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := computeBrokerConn(s)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}
}

func computeBrokerConn(s *Server) BrokerConnStatus {
	if s.cfg.NoBroker {
		return BrokerConnStatus{
			Connected: false,
			Mode:      "no_broker",
			Message:   "--no-broker 모드 — mymqd 와 통신하지 않음. broker 명령 / Tx 테스터 / Push 테스터 / WS 모니터는 503. mci-admin 을 broker 와 함께 재기동 필요.",
			Appl:      s.cfg.ApplName,
		}
	}
	cli := s.mq
	if cli == nil {
		return BrokerConnStatus{
			Connected: false,
			Mode:      "closed",
			Message:   "broker client 미초기화 (기동 실패 또는 종료 중).",
			Host:      s.cfg.BrokerHost,
			Port:      s.cfg.BrokerPort,
			Appl:      s.cfg.ApplName,
		}
	}
	if cli.Closed() {
		return BrokerConnStatus{
			Connected: false,
			Mode:      "closed",
			Message:   "broker client 종료됨 (재기동 필요).",
			Host:      s.cfg.BrokerHost,
			Port:      s.cfg.BrokerPort,
			Appl:      s.cfg.ApplName,
		}
	}
	if cli.Reconnecting() {
		return BrokerConnStatus{
			Connected: false,
			Mode:      "reconnecting",
			Message:   "broker 재연결 중 — mymqd 일시 끊김 가능. 잠시 후 자동 복구.",
			Host:      s.cfg.BrokerHost,
			Port:      s.cfg.BrokerPort,
			Appl:      s.cfg.ApplName,
		}
	}
	return BrokerConnStatus{
		Connected: true,
		Mode:      "ok",
		Message:   "broker 통신 정상.",
		Host:      s.cfg.BrokerHost,
		Port:      s.cfg.BrokerPort,
		Appl:      s.cfg.ApplName,
	}
}
