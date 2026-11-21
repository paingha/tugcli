// Package tui is tug's full-screen terminal UI. It works on the shared core
// rather than on kubectl: internal/ops lists the session's workloads,
// streams their logs and explains them, and a Backend from internal/cli
// connects the session and switches its namespace with the code the
// commands run. It draws with Bubble Tea and Lip Gloss; its model is driven
// by messages, so tests run it without a terminal.
package tui

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/session"
)

// Backend is what the TUI needs from the rest of tug. internal/cli provides
// it with the code its commands run, so connecting and switching namespace
// behave as tug connect and tug ns do. Its methods may be called from
// several goroutines at once.
type Backend interface {
	// Catalog loads the cluster catalog.
	Catalog() (*catalog.Catalog, error)
	// Session reads this terminal's session.
	Session() (session.State, error)
	// Connect points the session at a catalog cluster as tug connect does:
	// read-only by its labels, and through tugd when it has a tunnel.
	Connect(ctx context.Context, clusterID string) (session.State, error)
	// Namespaces lists the namespaces of the session's cluster.
	Namespaces(ctx context.Context) ([]string, error)
	// SetNamespace switches the session's namespace, as tug ns does.
	SetNamespace(ns string) (session.State, error)
	// Kube returns a client for the session's cluster. Its guard refuses
	// writes on a read-only session unless allowWrites says the user
	// agreed in the TUI; a client made with allowWrites is for that one
	// action.
	Kube(ctx context.Context, allowWrites bool) (kubernetes.Interface, error)
}

// DefaultRefresh is how often the workloads view reloads.
const DefaultRefresh = 5 * time.Second

// Option changes how Run behaves.
type Option func(*settings)

type settings struct {
	refresh time.Duration
	now     func() time.Time
}

// WithRefresh sets how often the workloads view reloads; zero turns
// reloading off.
func WithRefresh(d time.Duration) Option { return func(s *settings) { s.refresh = d } }

// WithNow sets the clock that ages and unlock times are measured with.
func WithNow(now func() time.Time) Option { return func(s *settings) { s.now = now } }

// Run shows the TUI on a terminal until the user quits or ctx ends, which is
// not an error. It starts in the workloads view when the session is
// connected and in the clusters view otherwise.
func Run(ctx context.Context, b Backend, in io.Reader, out io.Writer, opts ...Option) error {
	st, err := b.Session()
	if err != nil {
		return err
	}
	t := newTasks(ctx)
	defer t.stop()
	m := newModel(b, t, st, newStyles(out), opts...)
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithAltScreen(),
		// tug's own signal handling cancels ctx.
		tea.WithoutSignalHandler(),
	)
	_, err = p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// tasks owns the TUI's background work: the commands Bubble Tea runs on
// goroutines of its own, log streams and the refresh timer. Each gets the
// program's context, which Bubble Tea's Update has no way to pass along,
// and stop waits for all of them, so nothing outlives Run.
type tasks struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newTasks(parent context.Context) *tasks {
	ctx, cancel := context.WithCancel(parent)
	return &tasks{ctx: ctx, cancel: cancel}
}

// add counts a piece of work in, unless the tasks have stopped.
func (t *tasks) add() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.wg.Add(1)
	return true
}

// cmd returns a command that runs fn with the program's context.
func (t *tasks) cmd(fn func(ctx context.Context) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		if !t.add() {
			return nil
		}
		defer t.wg.Done()
		return fn(t.ctx)
	}
}

// start runs fn on a goroutine of its own, with a context that ends when
// the returned function is called or the program ends.
func (t *tasks) start(fn func(ctx context.Context)) (stop func()) {
	ctx, cancel := context.WithCancel(t.ctx)
	if !t.add() {
		cancel()
		return cancel
	}
	go func() {
		defer t.wg.Done()
		defer cancel()
		fn(ctx)
	}()
	return cancel
}

// stop cancels every task and waits for them to end.
func (t *tasks) stop() {
	t.cancel()
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.wg.Wait()
}
