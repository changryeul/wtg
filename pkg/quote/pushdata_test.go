package quote

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestEncodePushdataLayout(t *testing.T) {
	payload := []byte(`{"sym":"USDKRW","bid":1380.45,"ask":1380.55}`)
	buf, err := EncodePushdata(PushdataOptions{
		MarketID: 0x0102030405060708,
		SeqNum:   42,
		Symbol:   "USDKRW",
		Mask:     0xAABBCCDD,
		Type:     7,
		Flag:     3,
	}, payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(buf) != PushdataSize {
		t.Fatalf("len=%d, want %d", len(buf), PushdataSize)
	}
	if got := binary.BigEndian.Uint64(buf[0:8]); got != 0x0102030405060708 {
		t.Errorf("mkid=%x", got)
	}
	if got := binary.BigEndian.Uint32(buf[8:12]); got != 42 {
		t.Errorf("seqn=%d", got)
	}
	if got := strings.TrimRight(string(buf[12:12+LSymb]), "\x00"); got != "USDKRW" {
		t.Errorf("symb=%q", got)
	}
	off := 8 + 4 + LSymb
	if got := binary.BigEndian.Uint32(buf[off : off+4]); got != 0xAABBCCDD {
		t.Errorf("mask=%x", got)
	}
	off += 4
	if buf[off] != 7 {
		t.Errorf("type=%d", buf[off])
	}
	off++
	if buf[off] != 3 {
		t.Errorf("flag=%d", buf[off])
	}
	off++
	if got := binary.BigEndian.Uint16(buf[off : off+2]); int(got) != len(payload) {
		t.Errorf("msgl=%d, want %d", got, len(payload))
	}
	off += 2
	if !bytes.Equal(buf[off:off+len(payload)], payload) {
		t.Errorf("payload mismatch")
	}
	// 남은 영역은 NUL padding.
	for i := off + len(payload); i < PushdataSize; i++ {
		if buf[i] != 0 {
			t.Errorf("byte %d 가 NUL 이 아님: %x", i, buf[i])
			break
		}
	}
}

func TestEncodePushdataSymbolTruncate(t *testing.T) {
	long := strings.Repeat("X", 30) // LSymb=20 보다 김
	buf, err := EncodePushdata(PushdataOptions{Symbol: long}, []byte("p"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimRight(string(buf[12:12+LSymb]), "\x00")
	if len(got) != LSymb {
		t.Errorf("symb 길이=%d, want %d", len(got), LSymb)
	}
}

func TestEncodePushdataPayloadTooLong(t *testing.T) {
	if _, err := EncodePushdata(PushdataOptions{}, make([]byte, MaxPushLen+1)); err != ErrPushdataPayloadTooLong {
		t.Errorf("err=%v, want ErrPushdataPayloadTooLong", err)
	}
}

func TestEncodePushdataV1RoundTrip(t *testing.T) {
	env := JSONEnvelope{
		Sym: "USDKRW",
		Bid: 1380.45,
		Ask: 1380.55,
		TS:  time.Now().UTC(),
		Seq: 99,
	}
	buf, err := EncodePushdataV1(env)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(buf) != PushdataSize {
		t.Fatalf("len=%d", len(buf))
	}

	// msgb 영역에서 NUL trim 후 v1 envelope 으로 다시 디코드.
	off := 8 + 4 + LSymb + 4 + 1 + 1 + 2
	msgl := int(binary.BigEndian.Uint16(buf[off-2 : off]))
	body := buf[off : off+msgl]

	got, err := DecodeJSONEnvelope(body)
	if err != nil {
		t.Fatalf("decode v1: %v", err)
	}
	if got.Sym != env.Sym || got.Bid != env.Bid || got.Ask != env.Ask {
		t.Errorf("envelope round-trip mismatch: got %+v, want %+v", got, env)
	}

	// pushmsg.symb 도 envelope.Sym 으로 채워졌는지.
	if sym := strings.TrimRight(string(buf[12:12+LSymb]), "\x00"); sym != env.Sym {
		t.Errorf("pushmsg.symb=%q, want %q", sym, env.Sym)
	}
	if seqn := binary.BigEndian.Uint32(buf[8:12]); seqn != 99 {
		t.Errorf("pushmsg.seqn=%d, want 99", seqn)
	}
}
