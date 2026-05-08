package push

import (
	"log/slog"
	"sync"
)

// Registry 는 logon_id → []*Connection 매핑을 관리한다.
//
// 같은 사용자가 여러 단말 (web + 모바일 등) 로 동시에 접속할 수 있으므로
// 단일 logon_id 에 다수 Connection 이 매핑된다. 사용자별 fan-out 시 모든
// Connection 에 메시지를 전달한다.
//
// 동시성:
//   - mu (RWMutex) 보호 — Add/Remove 는 짧은 critical section.
//   - Send/Range 같은 read 작업은 RLock 사용.
type Registry struct {
	mu     sync.RWMutex
	byUsid map[string][]*Connection
	logger *slog.Logger
}

// NewRegistry 는 빈 Registry 를 생성한다.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		byUsid: make(map[string][]*Connection),
		logger: logger,
	}
}

// Add 는 새 Connection 을 등록한다. usid 가 비어있으면 panic (인증 미통과는
// 호출자가 사전 차단해야 한다).
func (r *Registry) Add(c *Connection) {
	if c.logonID == "" {
		panic("push.Registry.Add: empty logon_id")
	}
	r.mu.Lock()
	r.byUsid[c.logonID] = append(r.byUsid[c.logonID], c)
	r.mu.Unlock()
	r.logger.Info("connection 등록",
		slog.String("usid", c.logonID),
		slog.String("channel", c.channel),
		slog.Uint64("conn_id", c.id),
	)
}

// Remove 는 해당 Connection 을 제거한다 (이미 없으면 no-op).
// 일반적으로 Connection.Close() 의 onClose 콜백으로 호출됨.
func (r *Registry) Remove(c *Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.byUsid[c.logonID]
	for i, cc := range list {
		if cc == c {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(r.byUsid, c.logonID)
	} else {
		r.byUsid[c.logonID] = list
	}
	r.logger.Debug("connection 해제",
		slog.String("usid", c.logonID),
		slog.Uint64("conn_id", c.id),
	)
}

// FanoutToUser 는 단일 사용자의 모든 Connection 에 메시지를 전송한다.
// slow consumer 는 자동으로 Close 되어 다른 사용자 영향을 차단한다.
//
// 반환: 전송 성공한 connection 수, 실패(slow/closed)한 수.
func (r *Registry) FanoutToUser(usid string, payload []byte) (sent, failed int) {
	r.mu.RLock()
	conns := append([]*Connection(nil), r.byUsid[usid]...) // snapshot
	r.mu.RUnlock()

	for _, c := range conns {
		err := c.Send(payload)
		if err == nil {
			sent++
			continue
		}
		failed++
		// slow consumer 격리.
		if err == ErrSendQueueFull {
			r.logger.Warn("slow consumer 격리 — connection close",
				slog.String("usid", c.logonID),
				slog.Uint64("conn_id", c.id),
			)
			c.Close()
		}
	}
	return sent, failed
}

// FanoutBroadcast 는 모든 등록된 connection 에 동일 payload 를 전송한다.
// alert / 시스템 공지 등에 사용.
func (r *Registry) FanoutBroadcast(payload []byte) (sent, failed int) {
	r.mu.RLock()
	all := make([]*Connection, 0, len(r.byUsid)*2)
	for _, list := range r.byUsid {
		all = append(all, list...)
	}
	r.mu.RUnlock()

	for _, c := range all {
		err := c.Send(payload)
		if err == nil {
			sent++
		} else {
			failed++
			if err == ErrSendQueueFull {
				c.Close()
			}
		}
	}
	return sent, failed
}

// Count 는 등록된 connection 총 개수를 반환한다 (모니터링용).
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := 0
	for _, list := range r.byUsid {
		total += len(list)
	}
	return total
}

// UserCount 는 등록된 고유 사용자 수.
func (r *Registry) UserCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byUsid)
}

// ConnsForUser 는 특정 사용자의 connection 목록 snapshot.
func (r *Registry) ConnsForUser(usid string) []*Connection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.byUsid[usid]
	out := make([]*Connection, len(list))
	copy(out, list)
	return out
}

// CloseAll 은 등록된 모든 connection 을 종료한다 (서버 셧다운 시).
func (r *Registry) CloseAll() {
	r.mu.RLock()
	all := make([]*Connection, 0)
	for _, list := range r.byUsid {
		all = append(all, list...)
	}
	r.mu.RUnlock()
	for _, c := range all {
		c.Close()
	}
}
