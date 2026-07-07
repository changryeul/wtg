package price

import (
	"testing"
)

// TestLoadConfig_NoBroker_ForcesQuotePublishBrokerFalse — NoBroker=true 면
// QuotePublishBroker 가 자동 false 강제. broker 없는데 broker 로 publish
// 시도하면 매번 fail 하므로 사전 차단.
func TestLoadConfig_NoBroker_ForcesQuotePublishBrokerFalse(t *testing.T) {
	tests := []struct {
		name              string
		args              []string
		wantNoBroker      bool
		wantPublishBroker bool
	}{
		{
			name:              "default — broker 사용, publish 활성",
			args:              []string{},
			wantNoBroker:      false,
			wantPublishBroker: true,
		},
		{
			name:              "--no-broker → QuotePublishBroker 강제 false",
			args:              []string{"--no-broker"},
			wantNoBroker:      true,
			wantPublishBroker: false,
		},
		{
			name:              "--no-broker + --quote-publish-broker=true — NoBroker 가 우선",
			args:              []string{"--no-broker", "--quote-publish-broker=true"},
			wantNoBroker:      true,
			wantPublishBroker: false, // ← 강제
		},
		{
			name:              "broker 사용 + --quote-publish-broker=false (legacy 분리 옵션)",
			args:              []string{"--quote-publish-broker=false"},
			wantNoBroker:      false,
			wantPublishBroker: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(tt.args)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.NoBroker != tt.wantNoBroker {
				t.Errorf("NoBroker=%v, want %v", cfg.NoBroker, tt.wantNoBroker)
			}
			if cfg.QuotePublishBroker != tt.wantPublishBroker {
				t.Errorf("QuotePublishBroker=%v, want %v", cfg.QuotePublishBroker, tt.wantPublishBroker)
			}
		})
	}
}

// TestLoadConfig_NoBroker_EnvAlias — WTG_PRICE_NO_BROKER 환경변수 동작.
func TestLoadConfig_NoBroker_EnvAlias(t *testing.T) {
	t.Setenv("WTG_PRICE_NO_BROKER", "1")
	cfg, err := LoadConfig([]string{})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.NoBroker {
		t.Error("WTG_PRICE_NO_BROKER=1 → NoBroker=true 기대")
	}
	if cfg.QuotePublishBroker {
		t.Error("NoBroker=true → QuotePublishBroker 자동 false 기대")
	}
}
