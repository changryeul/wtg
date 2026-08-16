//go:build linux

package krxshm

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Mapped 는 mmap 된 SHM 파일 + Writer.
type Mapped struct {
	*Writer
	f   *os.File
	buf []byte
}

// Open 은 /dev/shm/mfsise (path)를 O_CREAT|O_RDWR 로 열고 ShmSize 로 맞춘 뒤 MAP_SHARED
// mmap. MAP_SHARED 라 trn AP(다른 mmap 리더)가 write 를 즉시 관측 (tmpfs 코히런트).
func Open(path string) (*Mapped, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, fmt.Errorf("krxshm open %s: %w", path, err)
	}
	if err := f.Truncate(ShmSize); err != nil {
		f.Close()
		return nil, fmt.Errorf("krxshm truncate: %w", err)
	}
	buf, err := syscall.Mmap(int(f.Fd()), 0, ShmSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("krxshm mmap: %w", err)
	}
	w, err := NewWriter(buf)
	if err != nil {
		syscall.Munmap(buf)
		f.Close()
		return nil, err
	}
	return &Mapped{Writer: w, f: f, buf: buf}, nil
}

// Sync — MS_SYNC msync (best-effort; MAP_SHARED tmpfs 는 이미 코히런트).
func (m *Mapped) Sync() error {
	_, _, e := syscall.Syscall(syscall.SYS_MSYNC,
		uintptr(unsafe.Pointer(&m.buf[0])), uintptr(len(m.buf)), uintptr(syscall.MS_SYNC))
	if e != 0 {
		return e
	}
	return nil
}

// Close — munmap + 파일 닫기.
func (m *Mapped) Close() error {
	err := syscall.Munmap(m.buf)
	if cerr := m.f.Close(); err == nil {
		err = cerr
	}
	return err
}
