package team

import (
	"fmt"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/guard"
)

// Rule is a policy and the team whose catalog sets it.
type Rule struct {
	Team string
	Policy
}

// Policies returns the policies of a team's catalog.
func Policies(team string, doc *Document) []Rule {
	out := make([]Rule, 0, len(doc.Policies))
	for _, p := range doc.Policies {
		out = append(out, Rule{Team: team, Policy: p})
	}
	return out
}

// Limits returns the safety limits for a session on cl. The read-only
// selectors from config.yaml see the cluster's labels as the user set
// them, as they always have. Team policies see them with the team's labels
// winning, so that a label set by hand cannot take a cluster out of a
// team's policy. Where several rules apply, the stricter wins: neither a
// team nor the user can make a cluster less safe than the other says.
func Limits(cl *catalog.Cluster, readOnlyLabels []map[string]string, policies []Rule) guard.Limits {
	var l guard.Limits
	labels := cl.EffectiveLabels()
	for _, sel := range readOnlyLabels {
		if guard.ReadOnlyFor(labels, []map[string]string{sel}) {
			l = l.Stricter(guard.Limits{ReadOnly: true, ReadOnlyBy: []string{"guard.readOnlyLabels " + selectorString(sel)}})
		}
	}
	teamLabels := cl.TeamLabels()
	for _, p := range policies {
		if !guard.ReadOnlyFor(teamLabels, []map[string]string{p.Selector}) {
			continue
		}
		by := fmt.Sprintf("policy %s of team %s", p.Name, p.Team)
		var pl guard.Limits
		if p.ReadOnly {
			pl.ReadOnly, pl.ReadOnlyBy = true, []string{by}
		}
		if p.MaxUnlock.Duration > 0 {
			pl.MaxUnlock, pl.MaxUnlockBy = p.MaxUnlock.Duration, by
		}
		l = l.Stricter(pl)
	}
	return l
}
