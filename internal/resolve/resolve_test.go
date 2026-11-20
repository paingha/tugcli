package resolve

import (
	"slices"
	"testing"
)

func TestBest(t *testing.T) {
	clusters := []string{"prod-eu", "prod-us", "staging-us", "kind-dev", "api", "api-server", "deploy/api-gateway"}
	tests := []struct {
		query string
		want  []string
	}{
		{"api", []string{"api"}},                  // exact beats prefix
		{"API", []string{"api"}},                  // case-insensitive exact
		{"prod", []string{"prod-eu", "prod-us"}},  // tie: caller asks
		{"prod-e", []string{"prod-eu"}},           // prefix
		{"us", []string{"prod-us", "staging-us"}}, // word prefix
		{"gate", []string{"deploy/api-gateway"}},  // substring
		{"stgus", []string{"staging-us"}},         // fuzzy
		{"zzz", nil},                              // nothing
		{"api-s", []string{"api-server"}},         // prefix over word prefix
		{"kd", []string{"kind-dev"}},              // fuzzy
		{"eu", []string{"prod-eu"}},               // word prefix
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			if got := Best(tt.query, clusters); !slices.Equal(got, tt.want) {
				t.Errorf("Best(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestFuzzyPrefersTighterMatches(t *testing.T) {
	got := All("pe", []string{"p-x-x-x-e", "pxe"})
	if len(got) != 2 || got[0].Value != "pxe" {
		t.Fatalf("All = %+v", got)
	}
}
