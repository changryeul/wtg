package routing

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// file_writeback.go — DevMode 의 file-backed Registry wrapper.
//
// in-memory Registry 의 Put/Delete/SetActive 를 인터셉트하고, 변경 후 dev-routes
// JSON 파일을 atomic 으로 다시 쓴다. 운영자가 UI 에서 추가/수정한 alias 가
// mci-admin 재시작 후에도 보존되게 한다.
//
// etcd 를 쓰는 운영 환경에선 사용 안 함 — etcd watch 가 영속화 책임. 본 wrapper
// 는 단일 인스턴스 dev/test 시나리오 한정.
//
// 설계 결정:
//   - wrapper 패턴 — Registry 인터페이스 동일, 내부에 raw Registry + file path 보관
//   - 매 변경마다 *전체* 룰 set 을 file 에 dump (delta merge 회피 — 단순/안전)
//   - atomic write — temp 파일 에 쓴 후 os.Rename (쓰기 중간 read 방지)
//   - mutex — 동시 변경 직렬화 (file write 충돌 방지)
//   - WatchRoutesFilePolicy 와의 race: self-write 는 watcher 가 재시드해도
//     additive 정책 + 동일 내용이라 no-op. 로그 noise 만 발생 (감수).

// FileBackedRegistry 는 raw Registry 를 감싸 변경 시 JSON file 에 write-back.
type FileBackedRegistry struct {
	Registry
	path   string
	logger *slog.Logger
	mu     sync.Mutex
}

// WrapWithFileWriteback 은 reg 를 FileBackedRegistry 로 감싼다.
// path 가 비어있으면 reg 그대로 반환 (no-op wrap).
//
// 호출 위치: dev-seed 가 끝난 직후 (시드 자체는 raw reg 에 적용해서 file write
// 노이즈 회피). 이후 운영자 변경 (PUT /v1/admin/routes 등) 만 파일 동기화.
func WrapWithFileWriteback(reg Registry, path string, logger *slog.Logger) Registry {
	if path == "" {
		return reg
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FileBackedRegistry{Registry: reg, path: path, logger: logger}
}

// Put — 위임 후 file flush.
func (r *FileBackedRegistry) Put(rule *Rule, updatedBy string) error {
	if err := r.Registry.Put(rule, updatedBy); err != nil {
		return err
	}
	r.flush("put:" + rule.Alias)
	return nil
}

// Delete — 위임 후 file flush.
func (r *FileBackedRegistry) Delete(alias string) error {
	if err := r.Registry.Delete(alias); err != nil {
		return err
	}
	r.flush("delete:" + alias)
	return nil
}

// SetActive — 위임 후 file flush.
func (r *FileBackedRegistry) SetActive(alias string, active bool, updatedBy string) error {
	if err := r.Registry.SetActive(alias, active, updatedBy); err != nil {
		return err
	}
	r.flush("active:" + alias)
	return nil
}

// flush — 현재 in-memory Registry 의 모든 룰을 JSON 으로 직렬화해 파일에 atomic write.
//
// 실패해도 in-memory 상태는 유지 — 운영자에게 warn 로그만 남김 (file 일시적
// permission 문제로 wholesale 롤백하면 더 혼란).
func (r *FileBackedRegistry) flush(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rules := r.Registry.List() // alias 정렬됨
	doc := dumpDoc{
		Comment: dumpComment,
		Routes:  rules,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		r.logger.Warn("dev-routes file flush 실패 (marshal)",
			slog.String("path", r.path), slog.Any("err", err))
		return
	}
	data = append(data, '\n')

	if err := atomicWriteFile(r.path, data, 0644); err != nil {
		r.logger.Warn("dev-routes file flush 실패 (write)",
			slog.String("path", r.path), slog.Any("err", err))
		return
	}
	r.logger.Info("dev-routes file flush",
		slog.String("path", r.path),
		slog.String("reason", reason),
		slog.Int("rules", len(rules)))
}

// dumpDoc — file 의 wire format. dev_seed.go 의 LoadRoutesFromFile 이 받는
// {"routes":[...]} 형식과 일치. _comment 는 사람이 읽는 hint (파싱 무시됨).
type dumpDoc struct {
	Comment string  `json:"_comment,omitempty"`
	Routes  []*Rule `json:"routes"`
}

const dumpComment = "WTG dev stack 라우팅 룰 — mci-admin 의 PUT/DELETE/POST 가 자동 갱신. " +
	"수동 편집도 가능 (WatchRoutesFilePolicy 가 2초 polling). 운영에선 etcd 가 source of truth."

// atomicWriteFile — temp 파일 에 쓴 후 os.Rename. 같은 디렉터리에 temp 생성해야
// rename 이 atomic (다른 filesystem 간 rename 은 fallback 발생).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".routes-*.json.tmp")
	if err != nil {
		return fmt.Errorf("temp file 생성: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 성공 시 noop

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("temp file write: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("temp file chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("temp file close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
