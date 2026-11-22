package guard

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLimitsStricter(t *testing.T) {
	tests := []struct {
		name      string
		a, b      Limits
		readOnly  bool
		maxUnlock time.Duration
		by        string
	}{
		{"neither", Limits{}, Limits{}, false, 0, ""},
		{"read-only wins", Limits{ReadOnly: true}, Limits{}, true, 0, ""},
		{"limit from the other", Limits{ReadOnly: true}, Limits{MaxUnlock: time.Hour, MaxUnlockBy: "b"}, true, time.Hour, "b"},
		{"shorter limit wins", Limits{MaxUnlock: time.Hour, MaxUnlockBy: "a"}, Limits{MaxUnlock: 10 * time.Minute, MaxUnlockBy: "b"}, false, 10 * time.Minute, "b"},
		{"longer limit loses", Limits{MaxUnlock: 10 * time.Minute, MaxUnlockBy: "a"}, Limits{MaxUnlock: time.Hour, MaxUnlockBy: "b"}, false, 10 * time.Minute, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.Stricter(tt.b)
			if got.ReadOnly != tt.readOnly || got.MaxUnlock != tt.maxUnlock || got.MaxUnlockBy != tt.by {
				t.Errorf("Stricter() = %+v, want readOnly %v, max %v by %q", got, tt.readOnly, tt.maxUnlock, tt.by)
			}
		})
	}
}

func TestLimitsUnlock(t *testing.T) {
	l := Limits{ReadOnly: true, MaxUnlock: 30 * time.Minute, MaxUnlockBy: "policy production-read-only of team platform"}
	if err := l.CheckUnlock("prod", 30*time.Minute); err != nil {
		t.Errorf("CheckUnlock(30m) = %v, want nil", err)
	}
	err := l.CheckUnlock("prod", time.Hour)
	var le *UnlockLimitError
	if !errors.As(err, &le) || le.Max != 30*time.Minute {
		t.Fatalf("CheckUnlock(1h) = %v, want an *UnlockLimitError for 30m", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "at most 30m") || !strings.Contains(msg, "team platform") || !strings.Contains(msg, "tug unlock 30m") {
		t.Errorf("CheckUnlock(1h) message = %q", msg)
	}
	if err := l.CheckReadWrite("prod"); !errors.As(err, &le) || !le.ReadWrite {
		t.Errorf("CheckReadWrite() = %v, want a refusal", err)
	}
	if err := (Limits{ReadOnly: true}).CheckReadWrite("prod"); err != nil {
		t.Errorf("CheckReadWrite() without a limit = %v, want nil", err)
	}
	if got := l.Unlock(time.Hour); got != 30*time.Minute {
		t.Errorf("Unlock(1h) = %v, want 30m", got)
	}
	if got := (Limits{}).Unlock(time.Hour); got != time.Hour {
		t.Errorf("Unlock(1h) without a limit = %v, want 1h", got)
	}
	if got := (Limits{MaxUnlock: 10 * time.Minute}).SuggestedUnlock(); got != 10*time.Minute {
		t.Errorf("SuggestedUnlock() under a 10m limit = %v, want 10m", got)
	}
	if got := l.SuggestedUnlock(); got != DefaultUnlock {
		t.Errorf("SuggestedUnlock() under a 30m limit = %v, want %v", got, DefaultUnlock)
	}
}

func TestDeniedErrorSuggestsAnUnlock(t *testing.T) {
	tests := []struct {
		unlock time.Duration
		want   string
	}{
		{0, "refused DELETE pods/api-0: prod is read-only; run `tug unlock 15m` to allow writes"},
		{10 * time.Minute, "refused DELETE pods/api-0: prod is read-only; run `tug unlock 10m` to allow writes"},
	}
	for _, tt := range tests {
		err := &DeniedError{Cluster: "prod", Action: "DELETE pods/api-0", Unlock: tt.unlock}
		if got := err.Error(); got != tt.want {
			t.Errorf("DeniedError{Unlock: %v}.Error() = %q, want %q", tt.unlock, got, tt.want)
		}
	}
}

func TestShortDuration(t *testing.T) {
	tests := map[time.Duration]string{
		30 * time.Minute:     "30m",
		time.Hour:            "1h",
		90 * time.Minute:     "1h30m",
		90 * time.Second:     "1m30s",
		15*time.Minute + 1e9: "15m1s",
		2*time.Hour + 30*1e9: "2h0m30s",
	}
	for d, want := range tests {
		if got := ShortDuration(d); got != want {
			t.Errorf("ShortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
