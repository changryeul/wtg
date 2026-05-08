package mymq

import (
	"bytes"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	in := Header{
		Length: 200,
		Func:   FCTran,
		Subc:   SubLogon,
		Nvia:   2,
		Dirf:   DirForward,
		Msgf:   MsgfHdr | MsgfCer,
		Ctlf:   CtlfNoc,
		Keyc:   KeySend,
		Ckey:   0xDEADBEEF,
		Clid:   MakeClid(MemberDms, 7, 0x123456),
		Errn:   0,

		CoffZipf: 2,
		CoffOff:  0x010000,
		SoffZipf: 0,
		SoffOff:  0x020000,

		ErrmLen: 12, ErrmOff: 100,
		PkeyLen: 8, PkeyOff: 116,
		NkeyLen: 4, NkeyOff: 124,

		BodyZipf: 0, BodyOff: 0x030000,
	}
	copy(in.Xchg[:], "FX_ORD")
	copy(in.Rkey[:], "ROUTE-KEY-1")
	copy(in.Wkey[:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	copy(in.Chan[:], []byte{'C', 'H', 'N', '1'})

	buf := make([]byte, HdrSize)
	if err := EncodeHeader(buf, &in); err != nil {
		t.Fatalf("EncodeHeader: %v", err)
	}
	got, err := DecodeHeader(buf)
	if err != nil {
		t.Fatalf("DecodeHeader: %v", err)
	}
	if got != in {
		t.Errorf("round-trip mismatch:\n  in =%+v\n  got=%+v", in, got)
	}
}

func TestHeaderWireFormat(t *testing.T) {
	// Verify exact byte positions for a known header. This is the safety net
	// against accidental field-order bugs that round-trip tests would miss.
	var h Header
	h.Length = 0x84
	h.Func = FCAdmin      // 3
	h.Subc = SubGetStatus // 150
	h.Nvia = 0
	h.Dirf = DirForward // 1
	h.Ckey = 0xDEADBEEF
	h.Clid = 0x12345678
	copy(h.Xchg[:], "ADMIN")
	h.BodyZipf = 0
	h.BodyOff = 0x000054

	buf := make([]byte, HdrSize)
	if err := EncodeHeader(buf, &h); err != nil {
		t.Fatal(err)
	}

	// Length field at offset 0..3 BE.
	if got := getU32(buf[0:4]); got != 0x84 {
		t.Errorf("length: got 0x%X want 0x84", got)
	}
	if buf[4] != byte(FCAdmin) {
		t.Errorf("func: got %d want %d", buf[4], FCAdmin)
	}
	if buf[5] != byte(SubGetStatus) {
		t.Errorf("subc: got %d want %d", buf[5], SubGetStatus)
	}
	if buf[7] != byte(DirForward) {
		t.Errorf("dirf: got %d want %d", buf[7], DirForward)
	}
	// xchg at offset 12..19
	if !bytes.HasPrefix(buf[12:20], []byte("ADMIN")) {
		t.Errorf("xchg position: got %q", buf[12:20])
	}
	// ckey at offset 36..39 BE
	if got := getU32(buf[36:40]); got != 0xDEADBEEF {
		t.Errorf("ckey: got 0x%X want 0xDEADBEEF", got)
	}
	// clid at offset 40..43 BE
	if got := getU32(buf[40:44]); got != 0x12345678 {
		t.Errorf("clid: got 0x%X want 0x12345678", got)
	}
	// body WHERE at offset 80..83: zipf=0 + 24-bit doff=0x000054
	if buf[80] != 0 {
		t.Errorf("body.zipf: got %d want 0", buf[80])
	}
	if got := getU24(buf[81:84]); got != 0x000054 {
		t.Errorf("body.doff: got 0x%X want 0x54", got)
	}
}

func TestWhereSzoffEncoding(t *testing.T) {
	// WHERE: zipf:1 + doff:3 (24-bit BE byte offset)
	t.Run("WHERE", func(t *testing.T) {
		buf := make([]byte, 4)
		encodeWhere(buf, 0x02, 0xABCDEF)
		if buf[0] != 0x02 || buf[1] != 0xAB || buf[2] != 0xCD || buf[3] != 0xEF {
			t.Errorf("encodeWhere: got % X", buf)
		}
		zipf, off := decodeWhere(buf)
		if zipf != 0x02 || off != 0xABCDEF {
			t.Errorf("decodeWhere: zipf=%d off=0x%X", zipf, off)
		}
	})

	// SZOFF: len:2 + off:2 (both 16-bit BE)
	t.Run("SZOFF", func(t *testing.T) {
		buf := make([]byte, 4)
		encodeSzoff(buf, 0x1234, 0x5678)
		if buf[0] != 0x12 || buf[1] != 0x34 || buf[2] != 0x56 || buf[3] != 0x78 {
			t.Errorf("encodeSzoff: got % X", buf)
		}
		ln, off := decodeSzoff(buf)
		if ln != 0x1234 || off != 0x5678 {
			t.Errorf("decodeSzoff: len=0x%X off=0x%X", ln, off)
		}
	})
}

func TestClidPacking(t *testing.T) {
	// ncid is 5 bits (0..31) due to bit-29 overlap with type field; see types.go.
	cases := []struct {
		mt, ncid, scid, want uint32
	}{
		{MemberLoc, 0, 0x123456, 0x00123456},
		{MemberDms, 7, 0xABCDEF, 0x27ABCDEF},
		{MemberNet, 31, 0xFFFFFF, 0x5FFFFFFF},
	}
	for _, c := range cases {
		got := MakeClid(c.mt, c.ncid, c.scid)
		if got != c.want {
			t.Errorf("MakeClid(%d,%d,0x%X) = 0x%X, want 0x%X",
				c.mt, c.ncid, c.scid, got, c.want)
		}
		mt, ncid, scid := SplitClid(got)
		if mt != c.mt || ncid != c.ncid || scid != c.scid {
			t.Errorf("SplitClid(0x%X) = (%d,%d,0x%X), want (%d,%d,0x%X)",
				got, mt, ncid, scid, c.mt, c.ncid, c.scid)
		}
	}

	// MakeClid masks ncid; values >= 32 should silently wrap.
	if got := MakeClid(MemberDms, 32, 0); got != MakeClid(MemberDms, 0, 0) {
		t.Errorf("ncid=32 should wrap to 0 (5-bit mask), got 0x%X", got)
	}
}

func TestNaviRoundTrip(t *testing.T) {
	n := Navi{Scid: 0xCAFEBABE, Iama: 0x12, Eatt: 1, Zipf: 2, Ncid: 5}
	copy(n.Xchg[:], "PRICE")
	copy(n.Rkey[:], "USDKRW")

	buf := make([]byte, NaviSize)
	EncodeNavi(buf, &n)
	got := DecodeNavi(buf)
	if got != n {
		t.Errorf("navi round-trip:\n  in =%+v\n  got=%+v", n, got)
	}
	// Verify scid is BE.
	if buf[LXchg+LRkey] != 0xCA || buf[LXchg+LRkey+3] != 0xBE {
		t.Errorf("scid not BE-encoded: % X", buf[LXchg+LRkey:LXchg+LRkey+4])
	}
}

func TestCookieRoundTrip(t *testing.T) {
	var c Cookie
	copy(c.Usid[:], "trader01")
	copy(c.Name[:], "Hong Gildong")
	copy(c.Maca[:], "00:11:22:33:44:55")
	copy(c.Pcip[:], "10.0.0.42")
	copy(c.Svip[:], "10.0.0.1")
	c.Clid = 0x11223344
	copy(c.Coki[:], []byte{0xDE, 0xAD, 0xBE, 0xEF})

	buf := make([]byte, CookieWire)
	EncodeCookie(buf, &c)
	got := DecodeCookie(buf)
	if got != c {
		t.Errorf("cookie round-trip mismatch")
	}
	// Clid must be BE at the right offset.
	clidOff := 16 + 12 + 24 + 20 + 20
	if v := getU32(buf[clidOff : clidOff+4]); v != 0x11223344 {
		t.Errorf("clid encoding: got 0x%X want 0x11223344", v)
	}
}

func TestEncodeFrameMinimal(t *testing.T) {
	in := &FrameInput{
		Func: FCAdmin,
		Subc: SubGetStatus,
		Dirf: DirForward,
		Keyc: KeySend,
		Xchg: "ADMIN",
		Ckey: 0xDEADBEEF,
		Body: []byte("status?"),
	}
	frame, err := EncodeFrame(in)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if int(getU32(frame[:4])) != len(frame) {
		t.Fatalf("length prefix mismatch: prefix=%d frame=%d",
			getU32(frame[:4]), len(frame))
	}

	df, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if df.Header.Func != FCAdmin || df.Header.Subc != SubGetStatus {
		t.Errorf("func/subc mismatch: %v/%v", df.Header.Func, df.Header.Subc)
	}
	if df.Header.Ckey != 0xDEADBEEF {
		t.Errorf("ckey: got 0x%X", df.Header.Ckey)
	}
	if !bytes.Equal(df.Body, []byte("status?")) {
		t.Errorf("body: got %q", df.Body)
	}
	if trimNulString(df.Header.Xchg[:]) != "ADMIN" {
		t.Errorf("xchg: got %q", trimNulString(df.Header.Xchg[:]))
	}
}

func TestEncodeFrameWithCookieAndError(t *testing.T) {
	var ck Cookie
	copy(ck.Usid[:], "trader01")
	ck.Clid = 0xCAFEBABE

	in := &FrameInput{
		Func:   FCTran,
		Subc:   SubTranErr,
		Dirf:   DirOrigin,
		Keyc:   KeySend,
		Xchg:   "FX",
		Rkey:   "ORD",
		Ckey:   42,
		Errn:   ErrAuth,
		ErrMsg: "Authentication failed",
		Cookie: &ck,
		Body:   bytes.Repeat([]byte("X"), 64),
	}
	frame, err := EncodeFrame(in)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	df, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if df.Header.Msgf&MsgfErr == 0 {
		t.Error("MsgfErr should be set when ErrMsg present")
	}
	if df.Header.Errn != ErrAuth {
		t.Errorf("errn: got %d", df.Header.Errn)
	}
	if df.ErrMsg != "Authentication failed" {
		t.Errorf("errm: got %q", df.ErrMsg)
	}
	if df.Cookie == nil {
		t.Fatal("cookie not parsed")
	}
	if df.Cookie.Clid != 0xCAFEBABE {
		t.Errorf("cookie.clid: got 0x%X", df.Cookie.Clid)
	}
	if trimNulString(df.Cookie.Usid[:]) != "trader01" {
		t.Errorf("cookie.usid: got %q", trimNulString(df.Cookie.Usid[:]))
	}
	if !bytes.Equal(df.Body, in.Body) {
		t.Error("body mismatch")
	}
}

func TestEncodeFrameWithNavis(t *testing.T) {
	navis := []Navi{
		{Scid: 1, Iama: 0x01},
		{Scid: 2, Iama: 0x10},
		{Scid: 3, Iama: 0x10},
	}
	copy(navis[0].Xchg[:], "ORIG")
	copy(navis[1].Xchg[:], "HOP1")
	copy(navis[2].Xchg[:], "DEST")

	in := &FrameInput{
		Func:  FCTran,
		Dirf:  DirForward,
		Navis: navis,
		Body:  []byte("payload"),
	}
	frame, err := EncodeFrame(in)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	df, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if int(df.Header.Nvia) != 3 {
		t.Errorf("nvia: got %d", df.Header.Nvia)
	}
	if len(df.Navis) != 3 {
		t.Fatalf("navis count: got %d", len(df.Navis))
	}
	for i := range navis {
		if df.Navis[i] != navis[i] {
			t.Errorf("navi[%d]: got %+v want %+v", i, df.Navis[i], navis[i])
		}
	}
}

func TestEncodeFrameTooManyNavis(t *testing.T) {
	in := &FrameInput{Func: FCTran, Navis: make([]Navi, MaxVia+1)}
	if _, err := EncodeFrame(in); err != ErrInvalidNavi {
		t.Errorf("expected ErrInvalidNavi, got %v", err)
	}
}

func TestDecodeFrameRejectsLengthMismatch(t *testing.T) {
	// Hand-build a header with declared length 200 but pass a 100-byte buffer.
	buf := make([]byte, 100)
	putU32(buf[0:4], 200)
	if _, err := DecodeFrame(buf); err == nil {
		t.Error("expected length mismatch error")
	}
}

func TestHeartbeatFrameLength(t *testing.T) {
	// The MyMQ heartbeat is a 4-byte frame: just length=4 with no body.
	hb := []byte{0, 0, 0, 4}
	if getU32(hb) != 4 {
		t.Fatal("heartbeat length encoding")
	}
}
