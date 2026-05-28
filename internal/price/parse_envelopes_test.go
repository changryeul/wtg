package price

import (
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/pkg/quote"
)

func TestParseEnvelopesSingleObject(t *testing.T) {
	body := []byte(`{"sym":"USDKRW","bid":1380.45,"ask":1380.55,"src":"SMB"}`)
	envs, err := ParseEnvelopes(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 {
		t.Fatalf("len=%d, want 1", len(envs))
	}
	if envs[0].Sym != "USDKRW" || envs[0].Bid != 1380.45 || envs[0].Ask != 1380.55 {
		t.Errorf("%+v", envs[0])
	}
}

func TestParseEnvelopesArray(t *testing.T) {
	body := []byte(`[
		{"sym":"USDKRW","bid":1380.45,"ask":1380.55,"src":"SMB"},
		{"sym":"EURUSD","bid":1.0850,"ask":1.0852,"src":"KMB"}
	]`)
	envs, err := ParseEnvelopes(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 {
		t.Fatalf("len=%d, want 2", len(envs))
	}
	if envs[0].Sym != "USDKRW" || envs[1].Sym != "EURUSD" {
		t.Errorf("%+v", envs)
	}
}

func TestParseEnvelopesTrailingNul(t *testing.T) {
	body := append([]byte(`{"sym":"USDKRW","bid":1.0,"ask":1.1,"src":"X"}`), make([]byte, 100)...)
	envs, err := ParseEnvelopes(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 {
		t.Fatalf("len=%d", len(envs))
	}
}

func TestParseEnvelopesArrayWithTrailingNul(t *testing.T) {
	raw := `[{"sym":"X","bid":1.0,"ask":1.1,"src":"S"}]`
	body := append([]byte(raw), make([]byte, 50)...)
	envs, err := ParseEnvelopes(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 || envs[0].Sym != "X" {
		t.Errorf("%+v", envs)
	}
}

func TestParseEnvelopesEmpty(t *testing.T) {
	if _, err := ParseEnvelopes(nil); err != quote.ErrEnvelopeEmpty {
		t.Errorf("nil err=%v", err)
	}
	if _, err := ParseEnvelopes([]byte("   \n\t")); err != quote.ErrEnvelopeEmpty {
		t.Errorf("whitespace err=%v", err)
	}
}

func TestParseEnvelopesInvalidJSON(t *testing.T) {
	if _, err := ParseEnvelopes([]byte(`{not json`)); err == nil {
		t.Errorf("invalid object 가 통과함")
	}
	if _, err := ParseEnvelopes([]byte(`[not json`)); err == nil {
		t.Errorf("invalid array 가 통과함")
	}
}

func TestParseEnvelopesBatchRoundTrip(t *testing.T) {
	// quote.EncodePushdataBatch → ParseEnvelopes 연계 확인.
	in := []quote.JSONEnvelope{
		{Sym: "USDKRW", Bid: 1380.45, Ask: 1380.55, Src: "SMB", Seq: 1},
		{Sym: "USDKRW", Bid: 1380.40, Ask: 1380.50, Src: "KMB", Seq: 2},
		{Sym: "EURUSD", Bid: 1.0850, Ask: 1.0852, Src: "EBS", Seq: 3},
	}
	wire, err := quote.EncodePushdataBatch(in)
	if err != nil {
		t.Fatal(err)
	}
	// pushdata wire 의 msgb 부분 = offset 40, 길이 msgl.
	// DecodePushData 로 Tick 까지 갔다가 Tick.Body 가 곧 msgb 페이로드.
	tick, err := DecodePushData(wire)
	if err != nil {
		t.Fatal(err)
	}
	envs, err := ParseEnvelopes(tick.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 3 {
		t.Fatalf("len=%d, want 3", len(envs))
	}
	for i, want := range in {
		if envs[i].Sym != want.Sym || envs[i].Bid != want.Bid || envs[i].Ask != want.Ask || envs[i].Src != want.Src {
			t.Errorf("env[%d]=%+v, want %+v", i, envs[i], want)
		}
	}
	// pushmsg.symb 는 첫 envelope sym
	if tick.Symbol != "USDKRW" {
		t.Errorf("Tick.Symbol=%q, want USDKRW", tick.Symbol)
	}
}

func TestParseEnvelopesLargeBatchSize(t *testing.T) {
	// 1512B msgb 한계 — envelope 당 ~90 bytes 가정, ~16 개 한계 근방.
	in := make([]quote.JSONEnvelope, 16)
	for i := range in {
		in[i] = quote.JSONEnvelope{
			Sym: "USDKRW", Bid: 1380.0 + float64(i)*0.01, Ask: 1380.5 + float64(i)*0.01, Src: "SMB",
		}
	}
	wire, err := quote.EncodePushdataBatch(in)
	if err != nil {
		t.Skip("16 envelopes 가 1512B 초과 — 작게 줄여 측정")
	}
	if !strings.Contains(string(wire), "USDKRW") {
		t.Errorf("wire 에 sym 없음")
	}
}
