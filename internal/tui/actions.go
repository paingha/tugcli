package tui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/paingha/tugcli/internal/ops"
)

// act runs the action a key asks for on a workload, from its row in the
// list or from one of its panes. Other keys do nothing.
func (m *model) act(msg tea.KeyMsg, w ops.WorkloadState) tea.Cmd {
	k := m.keys
	if m.kube == nil && key.Matches(msg, k.actions()...) {
		m.setWarning("Not connected to %s yet", m.state.ClusterName)
		return nil
	}
	switch {
	case key.Matches(msg, k.Logs):
		return m.openLogs(w)
	case key.Matches(msg, k.Why):
		return m.openReport(paneWhy, w)
	case key.Matches(msg, k.Tree):
		return m.openReport(paneTree, w)
	}
	return nil
}
