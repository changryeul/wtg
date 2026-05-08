package price

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecodePushDataRoundTrip(t *testing.T) {
	in := &Tick{
		MarketID: 0xCAFEBABE12345678,
		Symbol:   "USDKRW",
		SeqNum:   42,
		Mask:     0x0000FF00,
		Type:     2,
		Flag:     1,
		Body:     []byte(`{"bid":1300.5,"ask":1300.7}`),
	}
	raw := EncodePushData(in)
	if len(raw) != pushdataSize {
		t.Errorf("raw 길이: %d, want %d", len(raw), pushdataSize)
	}

	out, err := DecodePushData(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.MarketID != in.MarketID {
		t.Errorf("MarketID: got 0x%X want 0x%X", out.MarketID, in.MarketID)
	}
	if out.Symbol != in.Symbol {
		t.Errorf("Symbol: got %q want %q", out.Symbol, in.Symbol)
	}
	if out.SeqNum != in.SeqNum {
		t.Errorf("SeqNum: %d", out.SeqNum)
	}
	if out.Mask != in.Mask {
		t.Errorf("Mask: 0x%X", out.Mask)
	}
	if out.Type != in.Type || out.Flag != in.Flag {
		t.Errorf("Type/Flag: %d/%d", out.Type, out.Flag)
	}
	if !bytes.Equal(out.Body, in.Body) {
		t.Errorf("Body: got %q want %q", out.Body, in.Body)
	}
}

func TestDecodePushDataTooShort(t *testing.T) {
	short := make([]byte, pushdataSize-1)
	if _, err := DecodePushData(short); err != ErrTooShortPushData {
		t.Errorf("ErrTooShortPushData 기대, got %v", err)
	}
}

func TestDecodePushDataLongSymbolTrim(t *testing.T) {
	// 20 바이트 fixed 영역 안에 NUL 없이 가득 차면 그대로 string.
	in := &Tick{
		Symbol: "ABCDEFGHIJ",
		Body:   []byte("x"),
	}
	raw := EncodePushData(in)
	out, err := DecodePushData(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Symbol != "ABCDEFGHIJ" {
		t.Errorf("Symbol: %q", out.Symbol)
	}
	// strings 패키지에서 NUL trim 동작 검증.
	tail := strings.TrimRight(string(make([]byte, lSymb)), "\x00")
	if tail != "" {
		t.Errorf("NUL bytes 가 trim 안 됨")
	}
}

func TestDecodePushDataEmptyBody(t *testing.T) {
	in := &Tick{
		Symbol: "EURUSD",
		SeqNum: 1,
		Body:   nil,
	}
	raw := EncodePushData(in)
	out, err := DecodePushData(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body) != 0 {
		t.Errorf("Body 비어있어야 함: %v", out.Body)
	}
}

func TestDecodePushDataBodyOverflowClipped(t *testing.T) {
	// EncodePushData 는 1512 초과 시 자동 truncate.
	huge := make([]byte, maxPushLen+200)
	for i := range huge {
		huge[i] = byte(i)
	}
	in := &Tick{Symbol: "X", Body: huge}
	raw := EncodePushData(in)
	out, _ := DecodePushData(raw)
	if len(out.Body) != maxPushLen {
		t.Errorf("clipped body 길이: %d, want %d", len(out.Body), maxPushLen)
	}
	if !bytes.Equal(out.Body, huge[:maxPushLen]) {
		t.Error("clipped body 내용 불일치")
	}
}
