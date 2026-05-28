package mymq

import (
	"testing"
)

// TestSubDropsCounted — subCh 가 가득 찼을 때 deliverUnsolicited 가 silent
// drop 하지 않고 SubDrops() 카운터에 누적됨을 검증.
//
// 시나리오:
//   - Client 의 subCh 를 작은 크기(2)로 만들고 Subscribe() consume 하지 않음.
//   - deliverUnsolicited(fake df) 를 5회 호출 → 2 는 buffer 에 들어가고 3은 drop.
//   - SubDrops() == 3 이어야 한다.
func TestSubDropsCounted(t *testing.T) {
	c := &Client{subCh: make(chan *Unsolicited, 2)}

	for i := 0; i < 5; i++ {
		df := &DecodedFrame{
			Header: Header{Func: FCCast},
			Body:   []byte("payload"),
		}
		c.deliverUnsolicited(df)
	}

	if drops := c.SubDrops(); drops != 3 {
		t.Errorf("SubDrops()=%d, want 3 (subCh capacity 2, 5 deliveries → 2 in, 3 drop)", drops)
	}
}

// TestSubDropsZeroAtSteadyState — buffer 가 충분히 크면 drop 0 보장.
func TestSubDropsZeroAtSteadyState(t *testing.T) {
	// buffer 16 ≥ 10 deliveries → silent drop 트리거 안 됨.
	c := &Client{subCh: make(chan *Unsolicited, 16)}
	for i := 0; i < 10; i++ {
		c.deliverUnsolicited(&DecodedFrame{
			Header: Header{Func: FCCast},
			Body:   []byte("p"),
		})
	}
	if drops := c.SubDrops(); drops != 0 {
		t.Errorf("buffer 충분한데 SubDrops()=%d, want 0", drops)
	}
	if got := len(c.subCh); got != 10 {
		t.Errorf("subCh 큐 길이=%d, want 10 (모든 delivery 가 큐에 들어가야)", got)
	}
}

// TestOptionsSubBufferSize — Open 의 Options.SubBufferSize 가 채널 크기 결정.
// 0 이면 기존 default 256 로 유지 (회귀 보호).
func TestOptionsSubBufferSizeDefault(t *testing.T) {
	c := &Client{}
	applySubBufferDefault(&c.opts)
	if c.opts.SubBufferSize != 256 {
		t.Errorf("default SubBufferSize=%d, want 256", c.opts.SubBufferSize)
	}
}

func TestOptionsSubBufferSizeOverride(t *testing.T) {
	c := &Client{opts: Options{SubBufferSize: 8192}}
	applySubBufferDefault(&c.opts)
	if c.opts.SubBufferSize != 8192 {
		t.Errorf("override 무시됨: %d", c.opts.SubBufferSize)
	}
}
