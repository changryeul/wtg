package api

import (
	"strings"
	"testing"
)

func TestLoadConfigLoginModeChainRequiresSvcInc(t *testing.T) {
	_, err := LoadConfig([]string{"--login-mode=chain"})
	if err == nil || !strings.Contains(err.Error(), "svc-inc-dir") {
		t.Errorf("chain 은 svc-inc-dir 필수여야 함: %v", err)
	}
}

func TestLoadConfigLoginModeChainOK(t *testing.T) {
	cfg, err := LoadConfig([]string{"--login-mode=chain", "--svc-inc-dir=/tmp/inc"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoginMode != "chain" {
		t.Errorf("LoginMode=%q", cfg.LoginMode)
	}
}

func TestLoadConfigLoginModeInvalid(t *testing.T) {
	_, err := LoadConfig([]string{"--login-mode=banana"})
	if err == nil {
		t.Error("잘못된 login-mode 는 에러여야 함")
	}
}

func TestLoadConfigLoginModeDefaultLegacy(t *testing.T) {
	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoginMode != "" && cfg.LoginMode != "legacy" {
		t.Errorf("기본은 legacy: %q", cfg.LoginMode)
	}
}
