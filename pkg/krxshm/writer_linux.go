//go:build linux

package krxshm

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// mmapFile — path 를 O_CREAT|O_RDWR 로 열고 size 로 맞춘 뒤 MAP_SHARED mmap.
func mmapFile(path string, size int) (*os.File, []byte, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, nil, fmt.Errorf("krxshm open %s: %w", path, err)
	}
	if err := f.Truncate(int64(size)); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("krxshm truncate: %w", err)
	}
	buf, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("krxshm mmap: %w", err)
	}
	return f, buf, nil
}

func msync(buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	_, _, e := syscall.Syscall(syscall.SYS_MSYNC,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(syscall.MS_SYNC))
	if e != 0 {
		return e
	}
	return nil
}

// Mapped — mmap 된 파생 SHM(mfsise) + Writer.
type Mapped struct {
	*Writer
	f   *os.File
	buf []byte
}

// Open — /dev/shm/mfsise mmap (파생). MAP_SHARED 라 trn 리더가 write 즉시 관측.
func Open(path string) (*Mapped, error) {
	f, buf, err := mmapFile(path, ShmSize)
	if err != nil {
		return nil, err
	}
	w, err := NewWriter(buf)
	if err != nil {
		syscall.Munmap(buf)
		f.Close()
		return nil, err
	}
	return &Mapped{Writer: w, f: f, buf: buf}, nil
}

func (m *Mapped) Sync() error { return msync(m.buf) }
func (m *Mapped) Close() error {
	err := syscall.Munmap(m.buf)
	if cerr := m.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// BondMapped — mmap 된 채권 SHM(mbsise) + BondWriter.
type BondMapped struct {
	*BondWriter
	f   *os.File
	buf []byte
}

// OpenBond — /dev/shm/mbsise mmap (채권).
func OpenBond(path string) (*BondMapped, error) {
	f, buf, err := mmapFile(path, BondShmSize)
	if err != nil {
		return nil, err
	}
	w, err := NewBondWriter(buf)
	if err != nil {
		syscall.Munmap(buf)
		f.Close()
		return nil, err
	}
	return &BondMapped{BondWriter: w, f: f, buf: buf}, nil
}

func (m *BondMapped) Sync() error { return msync(m.buf) }
func (m *BondMapped) Close() error {
	err := syscall.Munmap(m.buf)
	if cerr := m.f.Close(); err == nil {
		err = cerr
	}
	return err
}
