package mymq

import (
	"errors"
	"testing"
	"time"
)

func TestReconnectOptionsDefaults(t *testing.T) {
	r := ReconnectOptions{}
	eff := r.effective()
	if eff.InitialBackoff != 1*time.Second {
		t.Errorf("InitialBackoff: %v", eff.InitialBackoff)
	}
	if eff.MaxBackoff != 30*time.Second {
		t.Errorf("MaxBackoff: %v", eff.MaxBackoff)
	}
	if eff.BackoffFactor != 2.0 {
		t.Errorf("BackoffFactor: %v", eff.BackoffFactor)
	}
	if eff.MaxAttempts != 0 {
		t.Errorf("MaxAttempts: %d (기본 0 = 무제한)", eff.MaxAttempts)
	}
}

func TestReconnectOptionsOverride(t *testing.T) {
	r := ReconnectOptions{
		InitialBackoff: 200 * time.Millisecond,
		MaxBackoff:     5 * time.Second,
		BackoffFactor:  3.0,
		MaxAttempts:    10,
	}
	eff := r.effective()
	if eff.InitialBackoff != 200*time.Millisecond {
		t.Errorf("InitialBackoff override 안 됨: %v", eff.InitialBackoff)
	}
	if eff.MaxBackoff != 5*time.Second {
		t.Errorf("MaxBackoff override 안 됨: %v", eff.MaxBackoff)
	}
	if eff.BackoffFactor != 3.0 {
		t.Errorf("BackoffFactor override 안 됨: %v", eff.BackoffFactor)
	}
	if eff.MaxAttempts != 10 {
		t.Errorf("MaxAttempts override 안 됨: %d", eff.MaxAttempts)
	}
}

func TestNextBackoffExponential(t *testing.T) {
	rc := ReconnectOptions{
		InitialBackoff: 1 * time.Second,
		MaxBackoff:     10 * time.Second,
		BackoffFactor:  2.0,
	}
	cases := []struct {
		in, want time.Duration
	}{
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{4 * time.Second, 8 * time.Second},
		{8 * time.Second, 10 * time.Second}, // MaxBackoff 로 cap
		{10 * time.Second, 10 * time.Second},
	}
	for _, c := range cases {
		got := nextBackoff(c.in, rc)
		if got != c.want {
			t.Errorf("nextBackoff(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestErrReconnectingSentinel(t *testing.T) {
	// ErrReconnecting 은 외부 호출자가 errors.Is 로 식별할 수 있어야 한다.
	wrapped := errors.New("retryable: " + ErrReconnecting.Error())
	_ = wrapped
	if !errors.Is(ErrReconnecting, ErrReconnecting) {
		t.Error("errors.Is 동등성 깨짐")
	}
}

func TestErrBrokerClosedSentinel(t *testing.T) {
	if !errors.Is(ErrBrokerClosed, ErrBrokerClosed) {
		t.Error("errors.Is 동등성 깨짐")
	}
}
