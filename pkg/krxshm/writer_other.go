//go:build !linux

package krxshm

import "errors"

// 비-linux 스텁 — SHM 적재는 linux 전용 (mmap /dev/shm). mac 개발/비-linux 빌드용.
type Mapped struct{ *Writer }

func Open(path string) (*Mapped, error) {
	return nil, errors.New("krxshm: SHM 적재는 linux 전용 (mci-price-krx 는 EC2 에서 구동)")
}
func (m *Mapped) Sync() error  { return nil }
func (m *Mapped) Close() error { return nil }

type BondMapped struct{ *BondWriter }

func OpenBond(path string) (*BondMapped, error) {
	return nil, errors.New("krxshm: 채권 SHM 적재는 linux 전용")
}
func (m *BondMapped) Sync() error  { return nil }
func (m *BondMapped) Close() error { return nil }
