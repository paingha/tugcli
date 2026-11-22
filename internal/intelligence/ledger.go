package intelligence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/filelock"
)

// Call is one request to a provider, as the usage ledger keeps it.
type Call struct {
	Time     time.Time `json:"time"`
	Provider string    `json:"provider"`
	// Model is the model that answered, which may be a fallback for the
	// one asked.
	Model  string `json:"model"`
	Target string `json:"target,omitempty"`
	Usage
	// CostUSD is nil when the model has no price.
	CostUSD *float64 `json:"costUSD"`
	// LatencyMS is from sending the request to reading the whole answer.
	LatencyMS int64  `json:"latencyMs"`
	RequestID string `json:"requestId,omitempty"`
}

// ledgerFile is the usage ledger's name in tug's data directory.
const ledgerFile = "intelligence-usage.json"

// keepCalls is how long the ledger keeps a call: long enough for a year of
// monthly totals.
const keepCalls = 400 * 24 * time.Hour

// Ledger is the local record of every call, in tug's data directory. It
// holds no prompt and no answer: only what each call took.
type Ledger struct {
	path string
	now  func() time.Time
}

// OpenLedger returns the ledger in dir, tug's data directory.
func OpenLedger(dir string, now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	return &Ledger{path: filepath.Join(dir, ledgerFile), now: now}
}

// Path is the ledger's file.
func (l *Ledger) Path() string { return l.path }

// Calls returns the calls in the ledger, oldest first.
func (l *Ledger) Calls() ([]Call, error) {
	b, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the usage ledger: %w", err)
	}
	var calls []Call
	if err := json.Unmarshal(b, &calls); err != nil {
		return nil, fmt.Errorf("reading the usage ledger %s: %w", l.path, err)
	}
	return calls, nil
}

// Record adds a call to the ledger, dropping calls older than a little over
// a year. Concurrent tug processes take turns.
func (l *Ledger) Record(c Call) error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	unlock, err := filelock.Lock(l.path + ".lock")
	if err != nil {
		return fmt.Errorf("locking the usage ledger: %w", err)
	}
	defer unlock()
	calls, err := l.Calls()
	if err != nil {
		return err
	}
	cutoff := l.now().Add(-keepCalls)
	calls = slices.DeleteFunc(calls, func(c Call) bool { return c.Time.Before(cutoff) })
	calls = append(calls, c)
	b, err := json.MarshalIndent(calls, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(l.path, append(b, '\n'), 0o600)
}

// Total sums the calls of one day or month.
type Total struct {
	// Period is a day, "2026-11-21", or a month, "2026-11", in UTC.
	Period string `json:"period"`
	Calls  int    `json:"calls"`
	Usage
	CostUSD float64 `json:"costUSD"`
	// UnpricedCalls counts calls whose cost is unknown, which CostUSD
	// leaves out.
	UnpricedCalls int `json:"unpricedCalls,omitempty"`
}

// Daily sums calls by UTC day, newest first.
func Daily(calls []Call) []Total { return totals(calls, "2006-01-02") }

// Monthly sums calls by UTC month, newest first.
func Monthly(calls []Call) []Total { return totals(calls, "2006-01") }

func totals(calls []Call, layout string) []Total {
	var out []Total
	index := map[string]int{}
	for _, c := range calls {
		period := c.Time.UTC().Format(layout)
		i, ok := index[period]
		if !ok {
			i = len(out)
			index[period] = i
			out = append(out, Total{Period: period})
		}
		t := &out[i]
		t.Calls++
		t.InputTokens += c.InputTokens
		t.OutputTokens += c.OutputTokens
		t.CacheReadTokens += c.CacheReadTokens
		t.CacheWriteTokens += c.CacheWriteTokens
		if c.CostUSD == nil {
			t.UnpricedCalls++
		} else {
			t.CostUSD += *c.CostUSD
		}
	}
	slices.SortFunc(out, func(a, b Total) int { return strings.Compare(b.Period, a.Period) })
	return out
}

// MonthSpend sums what the calls in now's UTC month cost.
func MonthSpend(calls []Call, now time.Time) Total {
	month := now.UTC().Format("2006-01")
	for _, t := range Monthly(calls) {
		if t.Period == month {
			return t
		}
	}
	return Total{Period: month}
}

// ErrOverCap is returned when a call could take the month's spend past the
// cap.
var ErrOverCap = errors.New("over the monthly spend cap")

// CheckCap refuses a call that could take this month's spend past the cap:
// when spent plus the most the call could cost passes it, or when the call
// has no price, since then tug cannot tell. A cap of zero allows any call.
// Its error says what to do.
func CheckCap(capUSD float64, spent Total, high float64, priced bool) error {
	if capUSD <= 0 {
		return nil
	}
	if !priced {
		return fmt.Errorf("%w: intelligence.monthlySpendCapUSD is $%.2f and this model has no price, so tug cannot tell what the call would cost; add its price with `tug settings add intelligence.prices model=<id> input=<$ per million> output=<$ per million>`",
			ErrOverCap, capUSD)
	}
	if spent.CostUSD+high <= capUSD {
		return nil
	}
	return fmt.Errorf("%w: $%.2f spent in %s and this call could cost up to $%.2f, past the cap of $%.2f; raise intelligence.monthlySpendCapUSD, lower intelligence.maxOutputTokens, use a cheaper model with --model, or wait for next month (UTC)",
		ErrOverCap, spent.CostUSD, spent.Period, high, capUSD)
}
