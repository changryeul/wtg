package krx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	wire "github.com/winwaysystems/wtg/pkg/krx"
)

var (
	errShort     = errors.New("krx: ingest 전문 2바이트 미만")
	errUnknownTR = errors.New("krx: 미지원 TR type")
)

// Server 는 선물시세 종목구독 ws fan-out + 전문 수신(ingest) 진입점.
// 배포 컴포넌트(mci-edge-krx)가 이걸 임베드한다. Stage 0 는 합성 ingest.
type Server struct {
	hub      *Hub
	logger   *slog.Logger
	upgrader websocket.Upgrader
	connSeq  atomic.Uint64
	mstats   McastStats
	masters  *MasterCache // 마스터(A006F/A001B) 캐시 — 체결 등락 enrichment 용
}

// NewServer — Hub + 로거.
func NewServer(logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		hub:      NewHub(),
		logger:   logger,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		masters:  NewMasterCache(),
	}
}

// Hub 접근 (테스트/메트릭).
func (srv *Server) Hub() *Hub { return srv.hub }

// control 은 web 클라의 구독 제어 메시지.
//
//	{"type":"subscribe","symbols":["101V6000"]}
//	{"type":"unsubscribe","symbols":["101V6000"]}
type control struct {
	Type    string   `json:"type"`
	Symbols []string `json:"symbols"`
}

// ServeWS — GET /v1/subscribe. ws 업그레이드 후 subscriber 등록, reader(구독제어)
// + writer(fan-out flush) 펌프 가동.
func (srv *Server) ServeWS(w http.ResponseWriter, r *http.Request) {
	ws, err := srv.upgrader.Upgrade(w, r, nil)
	if err != nil {
		srv.logger.Warn("ws upgrade 실패", slog.Any("error", err))
		return
	}
	id := "fut-" + itoa(srv.connSeq.Add(1))
	sub := NewSubscriber(id, 512)
	srv.hub.Add(sub)
	srv.logger.Info("futures ws 연결", slog.String("id", id), slog.Int("conns", srv.hub.Count()))

	done := make(chan struct{})
	// writer pump — 큐 → ws.
	go func() {
		defer close(done)
		for p := range sub.Out() {
			_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := ws.WriteMessage(websocket.TextMessage, p); err != nil {
				return
			}
		}
	}()

	// reader pump — 구독 제어. 종료 시 정리.
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}
		var c control
		if json.Unmarshal(msg, &c) != nil {
			continue
		}
		switch c.Type {
		case "subscribe":
			sub.Subscribe(c.Symbols)
		case "unsubscribe":
			sub.Unsubscribe(c.Symbols)
		}
	}

	srv.hub.Remove(id)
	sub.Close()
	<-done
	_ = ws.Close()
	srv.logger.Info("futures ws 종료", slog.String("id", id))
}

// Ingest — 전문 앞 2바이트(type)로 KA(체결)/KB(호가) 자동 판별 후 디코드·fan-out.
// 실 피드/합성 공통 진입점. 미지원 type 은 오류.
func (srv *Server) Ingest(b []byte) (string, int, int, error) {
	if len(b) < 2 {
		return "", 0, 0, errShort
	}
	// 마스터(원 KRX TR)는 앞 5바이트 TR코드로 먼저 판별 (KA/KB 등 push 2B type 보다 우선).
	if len(b) >= 5 {
		switch string(b[0:5]) {
		case "A306F": // (트랙2) 파생 체결 원 TR
			return srv.IngestA306F(b)
		case "B606F": // (트랙2) 파생 호가 원 TR
			return srv.IngestB606F(b)
		case "A006F": // 파생 종목정보 마스터
			return srv.IngestA006F(b)
		case "H306F": // 파생 정산가격
			return srv.IngestH306F(b)
		case "A301K": // (트랙2) 채권 체결 원 TR
			return srv.IngestA301K(b)
		case "B601K": // (트랙2) 채권 우선호가 원 TR
			return srv.IngestB601K(b)
		case "A001B": // 채권 종목정보 마스터
			return srv.IngestA001B(b)
		}
	}
	switch string(b[0:2]) {
	case "KA": // 선물/옵션 체결 (파생 공용)
		return srv.IngestKA(b)
	case "KB": // 선물/옵션 호가
		return srv.IngestKB(b)
	case "BA": // 채권 체결
		return srv.IngestBA(b)
	case "BB": // 채권 호가
		return srv.IngestBB(b)
	default:
		return "", 0, 0, errUnknownTR
	}
}

// IngestKA — KA(체결) 고정폭 전문 → fut.trade JSON 종목구독 fan-out.
func (srv *Server) IngestKA(b []byte) (string, int, int, error) {
	t, err := wire.DecodeKChe(b)
	if err != nil {
		return "", 0, 0, err
	}
	return srv.fanout(t.Code, t)
}

// IngestKB — KB(호가) 고정폭 전문 → fut.book JSON 종목구독 fan-out.
func (srv *Server) IngestKB(b []byte) (string, int, int, error) {
	fb, err := wire.DecodeKHoga(b)
	if err != nil {
		return "", 0, 0, err
	}
	return srv.fanout(fb.Code, fb)
}

// IngestBA — BA(채권 체결) 고정폭 전문 → bond.trade JSON 종목구독 fan-out.
func (srv *Server) IngestBA(b []byte) (string, int, int, error) {
	bt, err := wire.DecodeBACheg(b)
	if err != nil {
		return "", 0, 0, err
	}
	return srv.fanout(bt.Code, bt)
}

// IngestBB — BB(채권 호가) 고정폭 전문 → bond.book JSON 종목구독 fan-out.
func (srv *Server) IngestBB(b []byte) (string, int, int, error) {
	bb, err := wire.DecodeBBHoga(b)
	if err != nil {
		return "", 0, 0, err
	}
	return srv.fanout(bb.Code, bb)
}

// IngestA006F — A006F(파생 종목정보 마스터) → 캐시 저장 + fut.master fan-out.
// 전일종가(PrevClose)를 캐시해 후속 A306F 체결의 등락 계산에 쓴다.
func (srv *Server) IngestA006F(b []byte) (string, int, int, error) {
	m, err := wire.DecodeA006F(b)
	if err != nil {
		return "", 0, 0, err
	}
	srv.masters.PutFut(m)
	return srv.fanout(m.Code, m)
}

// IngestA306F — (트랙2) 원 파생 체결 A306F → 마스터 join enrichment → fut.trade fan-out.
// A306F 원 TR 에는 전일종가/등락이 없어, 캐시된 마스터(A006F)로 diff/rate/prevClose/sign 을 채운다.
func (srv *Server) IngestA306F(b []byte) (string, int, int, error) {
	ft, err := wire.DecodeA306F(b)
	if err != nil {
		return "", 0, 0, err
	}
	enrichFutTrade(ft, srv.masters.GetFut(ft.Code))    // 전일대비 (A006F)
	applyFutSettle(ft, srv.masters.GetSettle(ft.Code)) // 정산가 (H306F)
	return srv.fanout(ft.Code, ft)
}

// IngestH306F — (트랙2) 파생 정산가 H306F → 캐시 저장 + fut.settle fan-out.
// 정산가/최종결제가를 code 별로 캐시해 후속 A306F 체결의 settle 필드를 채운다.
func (srv *Server) IngestH306F(b []byte) (string, int, int, error) {
	s, err := wire.DecodeH306F(b)
	if err != nil {
		return "", 0, 0, err
	}
	srv.masters.PutSettle(s)
	return srv.fanout(s.Code, s)
}

// IngestB606F — (트랙2) 원 파생 호가 B606F → fut.book fan-out.
func (srv *Server) IngestB606F(b []byte) (string, int, int, error) {
	fb, err := wire.DecodeB606F(b)
	if err != nil {
		return "", 0, 0, err
	}
	return srv.fanout(fb.Code, fb)
}

// IngestA001B — A001B(채권 종목정보 마스터) → 캐시 저장 + bond.master fan-out.
// 기준가(BasePrc)를 캐시해 후속 A301K 체결의 전일대비 계산에 쓴다.
func (srv *Server) IngestA001B(b []byte) (string, int, int, error) {
	m, err := wire.DecodeA001B(b)
	if err != nil {
		return "", 0, 0, err
	}
	srv.masters.PutBond(m)
	return srv.fanout(m.Code, m)
}

// IngestA301K — (트랙2) 원 채권 체결 A301K → 직전대비(캐시)+전일대비(A001B) enrich → bond.trade.
func (srv *Server) IngestA301K(b []byte) (string, int, int, error) {
	bt, err := wire.DecodeA301K(b)
	if err != nil {
		return "", 0, 0, err
	}
	prev := srv.masters.BondPrevAndSet(bt.Code, bt.Last)
	enrichBondTrade(bt, srv.masters.GetBond(bt.Code), prev)
	return srv.fanout(bt.Code, bt)
}

// IngestB601K — (트랙2) 원 채권 우선호가 B601K → bond.book fan-out.
func (srv *Server) IngestB601K(b []byte) (string, int, int, error) {
	bb, err := wire.DecodeB601K(b)
	if err != nil {
		return "", 0, 0, err
	}
	return srv.fanout(bb.Code, bb)
}

// fanout — envelope 를 JSON 직렬화해 code 로 종목구독 fan-out.
func (srv *Server) fanout(code string, v interface{}) (string, int, int, error) {
	js, err := json.Marshal(v)
	if err != nil {
		return code, 0, 0, err
	}
	sent, dropped := srv.hub.BroadcastBySymbol(code, js)
	return code, sent, dropped, nil
}

// itoa — 작은 uint64 → 문자열 (fmt 회피용 경량).
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
