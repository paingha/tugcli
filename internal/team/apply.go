package team

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/config"
)

// What a Change is about.
const (
	KindAccount = "account"
	KindCluster = "cluster"
	KindIndex   = "plugin index"
	KindPolicy  = "policy"
)

// What a Change does.
const (
	OpAdd    = "add"
	OpUpdate = "update"
	OpRemove = "remove"
)

// Change is one thing that applying a team's catalog changes.
type Change struct {
	Op      string `json:"op"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail,omitempty"`
}

func (c Change) String() string {
	mark := map[string]string{OpAdd: "+", OpUpdate: "~", OpRemove: "-"}[c.Op]
	s := mark + " " + c.Kind + " " + c.Subject
	if c.Detail != "" {
		s += ": " + c.Detail
	}
	return s
}

// Conflict is a team entry that does not apply, or not in full, because a
// local entry or another team's wins over it.
type Conflict struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

func (c Conflict) String() string { return "! " + c.Kind + " " + c.Subject + ": " + c.Detail }

// Plan is what applying a version of a team's catalog changes.
type Plan struct {
	Team      string     `json:"team"`
	From      int64      `json:"from,omitempty"`
	To        int64      `json:"to"`
	Changes   []Change   `json:"changes,omitempty"`
	Conflicts []Conflict `json:"conflicts,omitempty"`
	// Waiting names the team's cluster entries that match no cluster in
	// the catalog yet. They apply when tug sync or tug import lists the
	// clusters.
	Waiting []string `json:"waiting,omitempty"`
}

func (p *Plan) change(op, kind, subject, detail string) {
	p.Changes = append(p.Changes, Change{Op: op, Kind: kind, Subject: subject, Detail: detail})
}

func (p *Plan) conflict(kind, subject, detail string) {
	p.Conflicts = append(p.Conflicts, Conflict{Kind: kind, Subject: subject, Detail: detail})
}

// Review applies next, a version of the team's catalog, to cat and returns
// everything it changes compared with prev, the version accepted before
// (nil for none), including the plugin indexes and policies that live
// outside the catalog. Nothing is saved: the caller saves cat, and accepts
// next, once the plan is approved.
func Review(cat *catalog.Catalog, team string, prev, next *Document) Plan {
	p := Apply(cat, team, next)
	if prev != nil {
		p.From = prev.Version
	}
	p.Changes = append(p.Changes, diffIndexes(team, prev, next)...)
	p.Changes = append(p.Changes, diffPolicies(prev, next)...)
	return p
}

// Apply merges a team's catalog into cat. It adds the team's accounts,
// names, aliases and labels the clusters its entries match, and takes back
// what an earlier version added that this one no longer lists. Entries the
// user made win over the team's, as do another team's; each such case is
// a Conflict. Every entry the team owns records the team and version.
func Apply(cat *catalog.Catalog, team string, doc *Document) Plan {
	p := Plan{Team: team, To: doc.Version}
	gone := "no longer in the team's catalog"
	applyAccounts(cat, team, doc, &p, gone)
	applyClusters(cat, team, doc, &p, gone)
	return p
}

// ApplyClusters is Apply for the team's cluster entries alone. It keeps the
// clusters a sync or an import lists named and labelled as an accepted
// version says, without bringing back an account the user has since
// removed; accounts change only when a version is reviewed.
func ApplyClusters(cat *catalog.Catalog, team string, doc *Document) Plan {
	p := Plan{Team: team, To: doc.Version}
	applyClusters(cat, team, doc, &p, "no longer in the team's catalog")
	return p
}

// Detach takes back everything a team's catalog added to cat, for when the
// team is removed. Accounts the team added stay as the user's own, since
// their clusters were listed with the user's credentials.
func Detach(cat *catalog.Catalog, team string) Plan {
	p := Plan{Team: team}
	gone := "team " + team + " is removed"
	applyAccounts(cat, team, &Document{}, &p, gone)
	applyClusters(cat, team, &Document{}, &p, gone)
	return p
}

func applyAccounts(cat *catalog.Catalog, team string, doc *Document, p *Plan, gone string) {
	listed := map[string]bool{}
	for _, a := range doc.Accounts {
		listed[a.Key()] = true
		prov := &catalog.Provenance{Team: team, Version: doc.Version}
		src, ok := cat.Source(a.Provider, a.Account)
		switch {
		case !ok:
			cat.Sources = append(cat.Sources, catalog.Source{Provider: a.Provider, Account: a.Account, Config: maps.Clone(a.Config), Team: prov})
			p.change(OpAdd, KindAccount, a.Key(), "settings "+settingsString(a.Config))
		case src.Team == nil:
			if !sameSettings(src.Config, a.Config) {
				p.conflict(KindAccount, a.Key(), fmt.Sprintf("you imported it yourself, and your settings (%s) win over the team's (%s)",
					settingsString(src.Config), settingsString(a.Config)))
			}
		case src.Team.Team != team:
			p.conflict(KindAccount, a.Key(), "team "+src.Team.Team+" set it up already, and its settings win")
		default:
			if !sameSettings(src.Config, a.Config) {
				p.change(OpUpdate, KindAccount, a.Key(), "settings "+settingsString(src.Config)+" → "+settingsString(a.Config))
				src.Config = maps.Clone(a.Config)
			}
			src.Team = prov
		}
	}
	for i := range cat.Sources {
		src := &cat.Sources[i]
		if src.Team != nil && src.Team.Team == team && !listed[src.Key()] {
			src.Team = nil
			p.change(OpRemove, KindAccount, src.Key(), gone+"; it stays as your own account, and tug forget "+src.Key()+" removes it and its clusters")
		}
	}
}

func applyClusters(cat *catalog.Catalog, team string, doc *Document, p *Plan, gone string) {
	// Keep what the team said before, to report what changes, and free
	// the aliases it gave so that this version can hand them out again.
	before := map[string]*catalog.TeamEntry{}
	for i := range cat.Clusters {
		cl := &cat.Clusters[i]
		if cl.Team != nil && cl.Team.Team == team {
			e := *cl.Team
			before[cl.ID] = &e
			cl.Team.Aliases = nil
		}
	}
	claimed := map[string]string{} // cluster ID: the entry that claimed it
	for _, e := range doc.Clusters {
		label := e.Name
		if label == "" {
			label = e.Match.String()
		}
		var matches []*catalog.Cluster
		for i := range cat.Clusters {
			if e.Match.Matches(&cat.Clusters[i]) {
				matches = append(matches, &cat.Clusters[i])
			}
		}
		if len(matches) == 0 {
			p.Waiting = append(p.Waiting, label)
			continue
		}
		primary := matches[0]
		if i := slices.IndexFunc(matches, func(cl *catalog.Cluster) bool { return !cl.Stale }); i >= 0 {
			primary = matches[i]
		}
		for _, m := range matches {
			switch {
			case m.Team != nil && m.Team.Team != team:
				p.conflict(KindCluster, m.Name, fmt.Sprintf("team %s describes it already, so the entry %s does not apply", m.Team.Team, label))
				continue
			case claimed[m.ID] != "":
				p.conflict(KindCluster, m.Name, fmt.Sprintf("the entries %s and %s both match it, and the first applies", claimed[m.ID], label))
				continue
			}
			claimed[m.ID] = label
			entry := &catalog.TeamEntry{Provenance: catalog.Provenance{Team: team, Version: doc.Version}, Labels: e.labels()}
			oldName := m.Name
			if m == primary {
				applyNames(cat, m, e, entry, p)
			} else {
				p.conflict(KindCluster, m.Name, fmt.Sprintf("it matches the entry %s, as %s does; it gets the team's labels but not its name", label, primary.Name))
			}
			for _, k := range mapsKeys(entry.Labels) {
				uv, ok := m.UserLabels[k]
				if !ok || uv == entry.Labels[k] {
					continue
				}
				mine := "your label " + k + "=" + uv
				if uv == "" {
					mine = "your removal of the label " + k
				}
				p.conflict(KindCluster, m.Name, fmt.Sprintf("%s wins over the team's %s=%s, though team policies still see %s=%s",
					mine, k, entry.Labels[k], k, entry.Labels[k]))
			}
			m.Team = entry
			if d := describeEntry(before[m.ID], entry, oldName, m.Name); d != "" {
				op := OpUpdate
				if before[m.ID] == nil {
					op = OpAdd
				}
				p.change(op, KindCluster, m.Name, d)
			}
		}
	}
	for i := range cat.Clusters {
		cl := &cat.Clusters[i]
		if cl.Team != nil && cl.Team.Team == team && claimed[cl.ID] == "" {
			cl.Team = nil
			p.change(OpRemove, KindCluster, cl.Name, gone+"; it keeps its name, and loses the team's labels and aliases")
		}
	}
	slices.SortFunc(cat.Clusters, func(a, b catalog.Cluster) int { return strings.Compare(a.Name, b.Name) })
}

// applyNames gives m the entry's name and aliases where no other cluster
// has them.
func applyNames(cat *catalog.Catalog, m *catalog.Cluster, e Cluster, entry *catalog.TeamEntry, p *Plan) {
	entry.Name = e.Name
	if e.Name != "" && e.Name != m.Name {
		if owner := cat.NameOwner(e.Name); owner != nil && owner != m {
			p.conflict(KindCluster, m.Name, fmt.Sprintf("the team names it %s, which %s already answers to; it keeps the name %s", e.Name, owner.Name, m.Name))
		} else {
			m.Name = e.Name
		}
	}
	for _, a := range e.Aliases {
		if a == m.Name {
			continue
		}
		if owner := cat.NameOwner(a); owner != nil && owner != m {
			p.conflict(KindCluster, m.Name, fmt.Sprintf("the alias %s is %s's already", a, owner.Name))
			continue
		}
		entry.Aliases = append(entry.Aliases, a)
		// Later entries must see the alias as taken.
		m.Team = entry
	}
}

// describeEntry says what changed between a cluster's team entries, or ""
// when nothing that people see did.
func describeEntry(before, after *catalog.TeamEntry, oldName, newName string) string {
	var parts []string
	if oldName != newName {
		parts = append(parts, "renamed from "+oldName)
	}
	var oldAliases []string
	var oldLabels map[string]string
	if before != nil {
		oldAliases, oldLabels = before.Aliases, before.Labels
	}
	if !slices.Equal(oldAliases, after.Aliases) {
		if len(after.Aliases) == 0 {
			parts = append(parts, "no aliases")
		} else {
			parts = append(parts, "aliases "+strings.Join(after.Aliases, ", "))
		}
	}
	if d := labelDiff(oldLabels, after.Labels); d != "" {
		parts = append(parts, "labels "+d)
	}
	return strings.Join(parts, "; ")
}

// labelDiff lists labels set or changed as key=value and labels taken
// away as -key.
func labelDiff(old, next map[string]string) string {
	var parts []string
	for _, k := range mapsKeys(next) {
		if v, ok := old[k]; !ok || v != next[k] {
			parts = append(parts, k+"="+next[k])
		}
	}
	for _, k := range mapsKeys(old) {
		if _, ok := next[k]; !ok {
			parts = append(parts, "-"+k)
		}
	}
	return strings.Join(parts, ",")
}

func sameSettings(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

// settingsString writes account settings as key=value pairs. Settings
// never hold credentials: Validate refuses names that look like them.
func settingsString(cfg map[string]any) string {
	if len(cfg) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := fmt.Sprint(cfg[k])
		_, isMap := cfg[k].(map[string]any)
		_, isList := cfg[k].([]any)
		if b, err := json.Marshal(cfg[k]); err == nil && (isMap || isList) {
			v = string(b)
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " ")
}

// IndexName is the name a team's plugin index has in tug: the team's name,
// a slash and the index's own name, so that it cannot be mistaken for an
// index in config.yaml.
func IndexName(team, index string) string { return team + "/" + index }

// Indexes returns the plugin indexes of a team's catalog under their names
// in tug.
func Indexes(team string, doc *Document) []config.PluginIndex {
	out := make([]config.PluginIndex, 0, len(doc.PluginIndexes))
	for _, ix := range doc.PluginIndexes {
		ix.Name = IndexName(team, ix.Name)
		out = append(out, ix)
	}
	return out
}

func diffIndexes(team string, prev, next *Document) []Change {
	old := map[string]config.PluginIndex{}
	if prev != nil {
		for _, ix := range prev.PluginIndexes {
			old[ix.Name] = ix
		}
	}
	describe := func(ix config.PluginIndex) string {
		key, err := ParsePublicKey(ix.PublicKey)
		if err != nil {
			return ix.URL
		}
		return ix.URL + ", signed by " + Fingerprint(key)
	}
	var out []Change
	for _, ix := range next.PluginIndexes {
		was, ok := old[ix.Name]
		delete(old, ix.Name)
		switch {
		case !ok:
			out = append(out, Change{Op: OpAdd, Kind: KindIndex, Subject: IndexName(team, ix.Name), Detail: describe(ix)})
		case was != ix:
			out = append(out, Change{Op: OpUpdate, Kind: KindIndex, Subject: IndexName(team, ix.Name), Detail: describe(ix)})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(old)) {
		out = append(out, Change{Op: OpRemove, Kind: KindIndex, Subject: IndexName(team, name)})
	}
	return out
}

func diffPolicies(prev, next *Document) []Change {
	old := map[string]Policy{}
	if prev != nil {
		for _, p := range prev.Policies {
			old[p.Name] = p
		}
	}
	var out []Change
	for _, p := range next.Policies {
		was, ok := old[p.Name]
		delete(old, p.Name)
		switch {
		case !ok:
			out = append(out, Change{Op: OpAdd, Kind: KindPolicy, Subject: p.Name, Detail: p.Summary()})
		case was.Summary() != p.Summary():
			out = append(out, Change{Op: OpUpdate, Kind: KindPolicy, Subject: p.Name, Detail: was.Summary() + " → " + p.Summary()})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(old)) {
		out = append(out, Change{Op: OpRemove, Kind: KindPolicy, Subject: name, Detail: old[name].Summary()})
	}
	return out
}
