//go:build !linux

package krxshm

import "errors"

// Mapped 스텁 — mci-price-krx SHM 적재는 linux 전용 (mmap /dev/shm). mac 개발/비-linux
// 빌드가 깨지지 않도록 no-op 스텁만 둔다.
type Mapped struct{ *Writer }

// Open — 비-linux 에서는 미지원.
func Open(path string) (*Mapped, error) {
	return nil, errors.New("krxshm: SHM 적재는 linux 전용 (mci-price-krx 는 EC2 에서 구동)")
}

// Sync / Close — 스텁.
func (m *Mapped) Sync() error  { return nil }
func (m *Mapped) Close() error { return nil }
