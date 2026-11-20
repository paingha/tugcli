// Package resolve turns what the user typed into names: exact beats prefix
// beats substring beats fuzzy, and ties are left for the caller to pick from.
package resolve

import (
	"sort"
	"strings"
)

// Rank orders match quality; lower is better.
type Rank int

const (
	// RankExact is an exact, case-sensitive match.
	RankExact Rank = iota + 1
	// RankExactFold matches ignoring case.
	RankExactFold
	// RankPrefix matches the start of the name.
	RankPrefix
	// RankWordPrefix matches the start of a word inside the name, for example
	// "api" in "deploy/api-server" or "eu" in "prod-eu".
	RankWordPrefix
	// RankSubstring matches anywhere in the name.
	RankSubstring
	// RankFuzzy matches the query's characters in order.
	RankFuzzy
	rankNone
)

// Match is one candidate that matched.
type Match struct {
	Value string
	Rank  Rank
	// Score breaks ties within a rank for fuzzy matches; lower is better.
	Score int
}

// All returns every candidate that matches query, best first.
func All(query string, candidates []string) []Match {
	var out []Match
	for _, c := range candidates {
		if r, score := rank(query, c); r != rankNone {
			out = append(out, Match{Value: c, Rank: r, Score: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// Best returns the candidates in the best matching rank. One result means the
// query is unambiguous; more than one means the caller should ask.
func Best(query string, candidates []string) []string {
	all := All(query, candidates)
	if len(all) == 0 {
		return nil
	}
	best := all[0]
	var out []string
	for _, m := range all {
		if m.Rank != best.Rank || (m.Rank == RankFuzzy && m.Score != best.Score) {
			break
		}
		out = append(out, m.Value)
	}
	return out
}

func rank(query, candidate string) (Rank, int) {
	if query == "" {
		return RankFuzzy, len(candidate)
	}
	if candidate == query {
		return RankExact, 0
	}
	q, c := strings.ToLower(query), strings.ToLower(candidate)
	switch {
	case c == q:
		return RankExactFold, 0
	case strings.HasPrefix(c, q):
		return RankPrefix, len(c) - len(q)
	case wordPrefix(c, q):
		return RankWordPrefix, len(c) - len(q)
	case strings.Contains(c, q):
		return RankSubstring, len(c) - len(q)
	}
	if gaps, ok := subsequence(c, q); ok {
		return RankFuzzy, gaps
	}
	return rankNone, 0
}

func wordPrefix(c, q string) bool {
	for i := 1; i < len(c); i++ {
		if isSep(c[i-1]) && strings.HasPrefix(c[i:], q) {
			return true
		}
	}
	return false
}

func isSep(b byte) bool {
	return b == '-' || b == '_' || b == '.' || b == '/' || b == ':' || b == '@'
}

// subsequence reports whether q's characters appear in c in order, and how
// many characters of c were skipped between the first and last match.
func subsequence(c, q string) (int, bool) {
	qi, first, last := 0, -1, -1
	for i := 0; i < len(c) && qi < len(q); i++ {
		if c[i] == q[qi] {
			if first < 0 {
				first = i
			}
			last = i
			qi++
		}
	}
	if qi < len(q) {
		return 0, false
	}
	return (last - first + 1) - len(q), true
}
