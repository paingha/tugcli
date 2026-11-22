package guard

import (
	"fmt"
	"strings"
	"time"
)

// Limits are what a cluster's safety rules say about a session on it:
// whether it opens read-only, and how long an unlock may last. They come
// from guard.readOnlyLabels in config.yaml and from team policies.
type Limits struct {
	ReadOnly bool
	// ReadOnlyBy names the rules that made the cluster read-only, such as
	// "policy production-read-only of team platform".
	ReadOnlyBy []string
	// MaxUnlock is the longest unlock allowed; zero means no limit.
	MaxUnlock time.Duration
	// MaxUnlockBy names the rule that set MaxUnlock.
	MaxUnlockBy string
}

// Stricter returns the stricter of l and o: read-only when either is, and
// the shorter unlock limit.
func (l Limits) Stricter(o Limits) Limits {
	out := l
	out.ReadOnly = l.ReadOnly || o.ReadOnly
	out.ReadOnlyBy = append(append([]string(nil), l.ReadOnlyBy...), o.ReadOnlyBy...)
	if o.MaxUnlock > 0 && (l.MaxUnlock == 0 || o.MaxUnlock < l.MaxUnlock) {
		out.MaxUnlock, out.MaxUnlockBy = o.MaxUnlock, o.MaxUnlockBy
	}
	return out
}

// Unlock returns d, shortened to the limit when there is one.
func (l Limits) Unlock(d time.Duration) time.Duration {
	if l.MaxUnlock > 0 {
		return min(d, l.MaxUnlock)
	}
	return d
}

// CheckUnlock returns an *UnlockLimitError when an unlock of d on cluster
// would last longer than the limit.
func (l Limits) CheckUnlock(cluster string, d time.Duration) error {
	if l.MaxUnlock > 0 && d > l.MaxUnlock {
		return &UnlockLimitError{Cluster: cluster, Max: l.MaxUnlock, By: l.MaxUnlockBy}
	}
	return nil
}

// CheckReadWrite returns an *UnlockLimitError when the rules limit unlocks
// on a read-only cluster: opening it read-write would be an unlock without
// an end.
func (l Limits) CheckReadWrite(cluster string) error {
	if l.ReadOnly && l.MaxUnlock > 0 {
		return &UnlockLimitError{Cluster: cluster, Max: l.MaxUnlock, By: l.MaxUnlockBy, ReadWrite: true}
	}
	return nil
}

// UnlockLimitError is returned for an unlock longer than a policy allows,
// and for opening read-write a cluster whose unlocks are limited.
type UnlockLimitError struct {
	Cluster string
	Max     time.Duration
	By      string
	// ReadWrite is set when the refusal is of --read-write.
	ReadWrite bool
}

func (e *UnlockLimitError) Error() string {
	limit := ShortDuration(e.Max)
	if e.ReadWrite {
		return fmt.Sprintf("%s cannot open read-write: %s limits unlocks to %s; connect without --read-write and run `tug unlock %s`",
			e.Cluster, e.By, limit, limit)
	}
	return fmt.Sprintf("an unlock on %s lasts at most %s, as %s says; run `tug unlock %s`", e.Cluster, limit, e.By, limit)
}

// ShortDuration writes d as people type it: 30m rather than 30m0s.
func ShortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
