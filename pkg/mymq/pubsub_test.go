package mymq

import (
	"bytes"
	"testing"
)

func TestBroadcastHeaderRoundTrip(t *testing.T) {
	var h BroadcastHeader
	copy(h.IPAddr[:], "10.0.0.42")
	copy(h.Exchange[:], "PRICE")
	copy(h.Chan[:], "CHN1")
	copy(h.User[:], "trader01")
	copy(h.LogonID[:], "USER0042")
	h.Function = uint8(FCCast)
	h.SubFunction = uint8(SubBroadcast)
	h.ViaNet = 1
	h.Debug = 0

	buf := make([]byte, BroadcastPrefixSize)
	EncodeBroadcastHeader(buf, &h)

	got, err := DecodeBroadcastHeader(buf)
	if err != nil {
		t.Fatalf("DecodeBroadcastHeader: %v", err)
	}
	if got != h {
		t.Errorf("round-trip mismatch:\n  in =%+v\n  got=%+v", h, got)
	}

	// Field accessors.
	if got.ExchangeString() != "PRICE" {
		t.Errorf("ExchangeString: %q", got.ExchangeString())
	}
	if got.LogonIDString() != "USER0042" {
		t.Errorf("LogonIDString: %q", got.LogonIDString())
	}
}

func TestSplitBroadcast(t *testing.T) {
	// Build an 80-byte prefix + arbitrary payload.
	var h BroadcastHeader
	copy(h.Exchange[:], "PRICE")
	copy(h.LogonID[:], "trader01")
	h.Function = uint8(FCPush)
	h.SubFunction = uint8(SubPush)

	body := make([]byte, BroadcastPrefixSize+12)
	EncodeBroadcastHeader(body[:BroadcastPrefixSize], &h)
	copy(body[BroadcastPrefixSize:], []byte("USDKRW=1234."))

	got, payload, err := SplitBroadcast(body)
	if err != nil {
		t.Fatalf("SplitBroadcast: %v", err)
	}
	if got == nil {
		t.Fatal("expected header")
	}
	if got.ExchangeString() != "PRICE" {
		t.Errorf("Exchange: %q", got.ExchangeString())
	}
	if !bytes.Equal(payload, []byte("USDKRW=1234.")) {
		t.Errorf("payload mismatch: %q", payload)
	}
}

func TestSplitBroadcastShortBody(t *testing.T) {
	// Body shorter than prefix should return nil header without error.
	short := []byte("hi")
	h, payload, err := SplitBroadcast(short)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if h != nil {
		t.Errorf("expected nil header, got %+v", h)
	}
	if !bytes.Equal(payload, short) {
		t.Errorf("payload should be passthrough")
	}
}
