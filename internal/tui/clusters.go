package tui

import (
	"context"
	"maps"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/session"
)

// catalogMsg brings the catalog.
type catalogMsg struct {
	clusters []catalog.Cluster
	err      error
}

// connectedMsg says how connecting the session went.
type connectedMsg struct {
	cluster catalog.Cluster
	state   session.State
	err     error
}

// clusterList is the clusters view: the catalog, filtered by what the user
// typed.
type clusterList struct {
	all     []catalog.Cluster
	loaded  bool
	err     error  // why the catalog did not load
	current string // the session's cluster ID
	// filter is what the user typed: words match names as tug connect
	// does, or label values, and label=value words select by label.
	filter    string
	filtering bool
	shown     []int // indexes into all
	cursor    int
	offset    int
	rows      int
}

func (m *model) loadCatalog() tea.Cmd {
	b := m.b
	return m.tasks.cmd(func(context.Context) tea.Msg {
		cat, err := b.Catalog()
		if err != nil {
			return catalogMsg{err: err}
		}
		return catalogMsg{clusters: cat.Clusters}
	})
}

func (l *clusterList) load(msg catalogMsg, current string) {
	l.err = msg.err
	if msg.err != nil {
		return
	}
	was := current
	if c := l.selected(); c != nil {
		was = c.ID
	}
	l.all, l.loaded, l.current = msg.clusters, true, current
	l.apply(was)
}

// apply filters the catalog again. The cursor goes to the cluster with ID
// was when it is still shown, and else to the best match.
func (l *clusterList) apply(was string) {
	l.shown = filterClusters(l.all, l.filter)
	l.cursor = 0
	for i, idx := range l.shown {
		if l.all[idx].ID == was {
			l.cursor = i
		}
	}
	l.scroll(l.rows)
}

// filterClusters returns the clusters a filter matches, best first: names
// ranked as tug connect ranks them, then clusters matched only by a label
// value, in catalog order.
func filterClusters(all []catalog.Cluster, filter string) []int {
	sel := map[string]string{}
	var words []string
	for _, w := range strings.Fields(filter) {
		if k, v, ok := strings.Cut(w, "="); ok {
			sel[k] = v
			continue
		}
		words = append(words, w)
	}
	type scored struct {
		idx  int
		rank resolve.Rank
	}
	var out []scored
	for i := range all {
		cl := &all[i]
		if !cl.Matches(sel) {
			continue
		}
		best := resolve.Rank(0)
		ok := true
		for j, w := range words {
			r, matched := wordRank(w, cl)
			if !matched {
				ok = false
				break
			}
			if j == 0 {
				best = r
			}
		}
		if ok {
			out = append(out, scored{idx: i, rank: best})
		}
	}
	slices.SortStableFunc(out, func(a, b scored) int { return int(a.rank) - int(b.rank) })
	idx := make([]int, len(out))
	for i, s := range out {
		idx[i] = s.idx
	}
	return idx
}

// labelRank ranks a match on a label value below every match on a name.
const labelRank = resolve.RankFuzzy + 1

// wordRank says how well a typed word matches a cluster: by its name, or
// failing that by a label value it is part of.
func wordRank(w string, cl *catalog.Cluster) (resolve.Rank, bool) {
	if m := resolve.All(w, []string{cl.Name}); len(m) > 0 {
		return m[0].Rank, true
	}
	lw := strings.ToLower(w)
	for _, v := range cl.EffectiveLabels() {
		if strings.Contains(strings.ToLower(v), lw) {
			return labelRank, true
		}
	}
	return 0, false
}

// byID finds a cluster in the catalog as last read.
func (l *clusterList) byID(id string) *catalog.Cluster {
	for i := range l.all {
		if l.all[i].ID == id {
			return &l.all[i]
		}
	}
	return nil
}

func (l *clusterList) selected() *catalog.Cluster {
	if l.cursor < 0 || l.cursor >= len(l.shown) {
		return nil
	}
	return &l.all[l.shown[l.cursor]]
}

func (l *clusterList) move(by int) {
	l.cursor = max(min(l.cursor+by, len(l.shown)-1), 0)
	l.scroll(l.rows)
}

// scroll keeps the cursor in view when rows fit on screen.
func (l *clusterList) scroll(rows int) {
	l.rows = rows
	l.offset = scrolled(l.cursor, l.offset, rows, len(l.shown))
}

// scrolled returns the first row to show so the cursor is in view.
func scrolled(cursor, offset, rows, total int) int {
	if rows <= 0 {
		return 0
	}
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+rows {
		offset = cursor - rows + 1
	}
	return max(min(offset, total-rows), 0)
}

// clustersKey handles a key in the clusters view. Typing filters; with
// nothing typed, q quits and ? shows help, and / starts a filter that may
// hold those too.
func (m *model) clustersKey(msg tea.KeyPressMsg) tea.Cmd {
	l := &m.clusters
	switch {
	case key.Matches(msg, m.keys.Connect):
		return m.connect()
	case key.Matches(msg, m.keys.Back):
		switch {
		case l.filter != "" || l.filtering:
			l.filter, l.filtering = "", false
			l.apply(l.current)
		case m.state.Connected():
			return m.enterWorkloads()
		}
		return nil
	case msg.String() == "up" || msg.String() == "ctrl+p":
		l.move(-1)
	case msg.String() == "down" || msg.String() == "ctrl+n":
		l.move(1)
	case msg.String() == "pgup":
		l.move(-l.rows)
	case msg.String() == "pgdown":
		l.move(l.rows)
	case msg.String() == "home":
		l.move(-len(l.shown))
	case msg.String() == "end":
		l.move(len(l.shown))
	case l.filter == "" && !l.filtering && key.Matches(msg, m.keys.Quit):
		return m.quit()
	case l.filter == "" && !l.filtering && key.Matches(msg, m.keys.Help):
		m.openHelp()
	case l.filter == "" && !l.filtering && key.Matches(msg, m.keys.Filter):
		l.filtering = true
	case edit(&l.filter, msg):
		l.apply("") // the best match
	}
	return nil
}

// connect points the session at the selected cluster.
func (m *model) connect() tea.Cmd {
	cl := m.clusters.selected()
	if cl == nil {
		return nil
	}
	c := *cl
	m.setInfo("Connecting to %s…", c.Name)
	b := m.b
	return m.tasks.cmd(func(ctx context.Context) tea.Msg {
		st, err := b.Connect(ctx, c.ID)
		return connectedMsg{cluster: c, state: st, err: err}
	})
}

func (m *model) connected(msg connectedMsg) tea.Cmd {
	if msg.err != nil {
		m.setError(msg.err)
		return nil
	}
	m.state = msg.state
	m.clusters.current = msg.state.ClusterID
	m.kube, m.kubeFor = nil, ""
	m.workloads = workloadList{}
	mode := "read-write"
	if msg.state.ReadOnly {
		mode = "read-only (tug unlock 15m to allow writes)"
	}
	m.setInfo("Connected to %s, namespace %s, %s", msg.state.ClusterName, msg.state.Namespace, mode)
	if msg.cluster.Stale {
		m.setWarning("Connected to %s, %s; this cluster was not listed by its provider at the last sync", msg.state.ClusterName, mode)
	}
	return m.enterWorkloads()
}

// clustersView draws the catalog.
func (m *model) clustersView(height int) string {
	l := &m.clusters
	var b strings.Builder
	prompt := "Type to filter"
	if l.filter != "" || l.filtering {
		prompt = "Filter: " + l.filter + "▏"
	}
	b.WriteString(m.st.dim.Render(prompt) + "\n")
	switch {
	case l.err != nil:
		b.WriteString(m.st.red.Render(truncate(errorText(l.err), m.width)))
		return b.String()
	case !l.loaded:
		b.WriteString("Loading the catalog…")
		return b.String()
	case len(l.all) == 0:
		b.WriteString("No clusters. Quit and run `tug import` to find some.")
		return b.String()
	case len(l.shown) == 0:
		b.WriteString("No cluster matches. esc clears the filter.")
		return b.String()
	}
	rows := make([][]string, len(l.shown))
	for i, idx := range l.shown {
		cl := &l.all[idx]
		labels := cl.EffectiveLabels()
		marker := " "
		if cl.ID == l.current {
			marker = "*"
		}
		status := ""
		if cl.Stale {
			status = "stale"
		}
		rows[i] = []string{marker, cl.Name, cl.Provider, dash(cl.Region), dash(labels[catalog.LabelEnv]), status, extraLabels(labels)}
	}
	t := table{
		columns: []column{{"", 1}, {"NAME", 40}, {"PROVIDER", 14}, {"REGION", 16}, {"ENV", 10}, {"STATUS", 5}, {"LABELS", 0}},
		rows:    rows,
		style: func(row, col int, cell string) string {
			switch {
			case col == 4 && rows[row][4] == "prod":
				return m.st.red.Render(cell)
			case col == 5:
				return m.st.yellow.Render(cell)
			case col == 6:
				return m.st.dim.Render(cell)
			}
			return cell
		},
	}
	ws := t.widths(m.width - 2)
	b.WriteString("  " + m.st.header.Render(t.headingLine(ws)) + "\n")
	l.scroll(height - 2)
	end := min(l.offset+height-2, len(rows))
	for i := l.offset; i < end; i++ {
		b.WriteString(t.row(m.st, ws, i, i == l.cursor) + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// extraLabels shows the labels without a column of their own, as tug ls
// does.
func extraLabels(labels map[string]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		switch k {
		case catalog.LabelProvider, catalog.LabelAccount, catalog.LabelRegion, catalog.LabelEnv:
			continue
		}
		out = append(out, k+"="+labels[k])
	}
	return strings.Join(out, ",")
}
