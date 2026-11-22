package intelligence

import (
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func cost(v float64) *float64 { return &v }

func TestLedger(t *testing.T) {
	dir := t.TempDir()
	clock := now
	l := OpenLedger(dir, func() time.Time { return clock })
	if calls, err := l.Calls(); err != nil || calls != nil {
		t.Fatalf("Calls of a new ledger = %v, %v", calls, err)
	}
	old := Call{Time: now.AddDate(-2, 0, 0), Provider: Anthropic, Model: "claude-opus-5-5", CostUSD: cost(9)}
	calls := []Call{
		old,
		{Time: time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), Provider: OpenAI, Model: "gpt-6.2-astra", Usage: Usage{InputTokens: 100, OutputTokens: 10}, CostUSD: cost(1.5)},
		{Time: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), Provider: Anthropic, Model: "claude-opus-5-5", Usage: Usage{InputTokens: 200, OutputTokens: 20, CacheReadTokens: 50}, CostUSD: cost(0.25), RequestID: "req_1"},
		{Time: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC), Provider: Custom, Model: "llama4", Usage: Usage{InputTokens: 300, OutputTokens: 30}},
		{Time: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC), Provider: Anthropic, Model: "claude-opus-5-5", Usage: Usage{InputTokens: 400, OutputTokens: 40}, CostUSD: cost(0.5)},
	}
	for _, c := range calls {
		if err := l.Record(c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.Calls()
	if err != nil {
		t.Fatal(err)
	}
	// The call from two years ago is gone; the rest are kept in order.
	if len(got) != 4 || !got[0].Time.Equal(calls[1].Time) || got[1].RequestID != "req_1" || got[2].CostUSD != nil {
		t.Errorf("Calls = %+v", got)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(l.Path()); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("ledger mode = %v, %v, want 0600", fi.Mode().Perm(), err)
		}
	}
	b, _ := os.ReadFile(l.Path())
	if strings.Contains(string(b), "prompt") || strings.Contains(string(b), "evidence") {
		t.Errorf("the ledger holds more than usage:\n%s", b)
	}

	wantDaily := []Total{
		{Period: "2026-11-21", Calls: 2, Usage: Usage{InputTokens: 700, OutputTokens: 70}, CostUSD: 0.5, UnpricedCalls: 1},
		{Period: "2026-11-20", Calls: 1, Usage: Usage{InputTokens: 200, OutputTokens: 20, CacheReadTokens: 50}, CostUSD: 0.25},
		{Period: "2026-10-31", Calls: 1, Usage: Usage{InputTokens: 100, OutputTokens: 10}, CostUSD: 1.5},
	}
	if d := Daily(got); !reflect.DeepEqual(d, wantDaily) {
		t.Errorf("Daily = %+v\nwant %+v", d, wantDaily)
	}
	wantMonthly := []Total{
		{Period: "2026-11", Calls: 3, Usage: Usage{InputTokens: 900, OutputTokens: 90, CacheReadTokens: 50}, CostUSD: 0.75, UnpricedCalls: 1},
		{Period: "2026-10", Calls: 1, Usage: Usage{InputTokens: 100, OutputTokens: 10}, CostUSD: 1.5},
	}
	if m := Monthly(got); !reflect.DeepEqual(m, wantMonthly) {
		t.Errorf("Monthly = %+v\nwant %+v", m, wantMonthly)
	}
	if s := MonthSpend(got, now); s.Period != "2026-11" || s.CostUSD != 0.75 {
		t.Errorf("MonthSpend = %+v", s)
	}
	if s := MonthSpend(got, time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)); s.Period != "2027-01" || s.CostUSD != 0 || s.Calls != 0 {
		t.Errorf("MonthSpend in a month without calls = %+v", s)
	}
}

func TestLedgerTakesTurns(t *testing.T) {
	l := OpenLedger(t.TempDir(), nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := l.Record(Call{Time: time.Now(), Provider: Anthropic, Model: "claude-opus-5-5", CostUSD: cost(0.01)}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls, err := l.Calls(); err != nil || len(calls) != 8 {
		t.Errorf("after 8 concurrent records, Calls = %d, %v", len(calls), err)
	}
}

func TestCheckCap(t *testing.T) {
	spent := Total{Period: "2026-11", CostUSD: 9.5}
	tests := []struct {
		name   string
		capUSD float64
		high   float64
		priced bool
		want   string
	}{
		{name: "no cap", high: 100, priced: false},
		{name: "under it", capUSD: 10, high: 0.5, priced: true},
		{
			name: "past it", capUSD: 10, high: 0.75, priced: true,
			want: "over the monthly spend cap: $9.50 spent in 2026-11 and this call could cost up to $0.75, past the cap of $10.00; raise intelligence.monthlySpendCapUSD, lower intelligence.maxOutputTokens, use a cheaper model with --model, or wait for next month (UTC)",
		},
		{
			name: "no price under a cap", capUSD: 10, high: 0, priced: false,
			want: "over the monthly spend cap: intelligence.monthlySpendCapUSD is $10.00 and this model has no price, so tug cannot tell what the call would cost; add its price under intelligence.prices in config.yaml",
		},
	}
	for _, tt := range tests {
		err := CheckCap(tt.capUSD, spent, tt.high, tt.priced)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: CheckCap = %v, want nil", tt.name, err)
		case tt.want != "" && (err == nil || err.Error() != tt.want || !errors.Is(err, ErrOverCap)):
			t.Errorf("%s: CheckCap = %v, want %q", tt.name, err, tt.want)
		}
	}
}
