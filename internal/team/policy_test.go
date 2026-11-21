package team

import (
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/config"
)

func TestLimits(t *testing.T) {
	policies := []Rule{
		{Team: "platform", Policy: Policy{Name: "production-read-only", Selector: map[string]string{"env": "prod"}, ReadOnly: true,
			MaxUnlock: config.Duration{Duration: 30 * time.Minute}}},
		{Team: "platform", Policy: Policy{Name: "staging-write", Selector: map[string]string{"env": "staging"}}},
		{Team: "infra", Policy: Policy{Name: "payments", Selector: map[string]string{"team": "payments"}, ReadOnly: true,
			MaxUnlock: config.Duration{Duration: 10 * time.Minute}}},
	}
	defaults := []map[string]string{{"env": "prod"}}
	tests := []struct {
		name      string
		cl        catalog.Cluster
		local     []map[string]string
		readOnly  bool
		maxUnlock time.Duration
		by        string
	}{
		{
			name: "no policy", cl: catalog.Cluster{Labels: map[string]string{"env": "dev"}}, local: defaults,
		},
		{
			name: "a provider's label meets the team's policy", cl: catalog.Cluster{Labels: map[string]string{"env": "prod"}}, local: defaults,
			readOnly: true, maxUnlock: 30 * time.Minute, by: "policy production-read-only of team platform",
		},
		{
			name:     "team label makes it production",
			cl:       catalog.Cluster{Labels: map[string]string{"env": "dev"}, Team: teamEntry(map[string]string{"env": "prod"})},
			readOnly: true, maxUnlock: 30 * time.Minute, by: "policy production-read-only of team platform",
		},
		{
			name: "a label of the user's cannot escape the team's policy",
			cl: catalog.Cluster{Labels: map[string]string{"env": "dev"}, Team: teamEntry(map[string]string{"env": "prod"}),
				UserLabels: map[string]string{"env": "staging"}},
			local:    defaults,
			readOnly: true, maxUnlock: 30 * time.Minute, by: "policy production-read-only of team platform",
		},
		{
			name: "a label of the user's still corrects a provider's guess",
			cl:   catalog.Cluster{Labels: map[string]string{"env": "prod"}, UserLabels: map[string]string{"env": "dev"}},
			// The team's policy sees env=dev too, since the team did not set env.
			local: defaults,
		},
		{
			name:  "the user can be stricter than the team",
			cl:    catalog.Cluster{Labels: map[string]string{"env": "staging"}, UserLabels: map[string]string{"tier": "critical"}},
			local: []map[string]string{{"tier": "critical"}}, readOnly: true,
		},
		{
			name:     "the shorter limit wins",
			cl:       catalog.Cluster{Team: teamEntry(map[string]string{"env": "prod", "team": "payments"})},
			readOnly: true, maxUnlock: 10 * time.Minute, by: "policy payments of team infra",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Limits(&tt.cl, tt.local, policies)
			if got.ReadOnly != tt.readOnly || got.MaxUnlock != tt.maxUnlock || got.MaxUnlockBy != tt.by {
				t.Errorf("Limits() = %+v, want readOnly %v, max %v by %q", got, tt.readOnly, tt.maxUnlock, tt.by)
			}
		})
	}
}

func teamEntry(labels map[string]string) *catalog.TeamEntry {
	return &catalog.TeamEntry{Provenance: catalog.Provenance{Team: "platform", Version: 1}, Labels: labels}
}
