// Package session keeps per-terminal state. A session is a directory holding a
// kubeconfig with exactly one context, plus the environment variables the
// shell hook exports. Other terminals and ~/.kube/config are never touched.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/paingha/tugcli/internal/config"
)

// Environment variables set by the shell hook.
const (
	EnvSession         = "TUG_SESSION"
	EnvSessionPID      = "TUG_SESSION_PID"
	EnvSessionDir      = "TUG_SESSION_DIR"
	EnvOrigKubeconfig  = "TUG_ORIG_KUBECONFIG"
	kubeconfigFileName = "kubeconfig"
	stateFileName      = "state.json"
	promptFileName     = "prompt"
)

// ErrNoSession is returned when the terminal has no tug session.
var ErrNoSession = errors.New("no tug session in this terminal")

var validID = regexp.MustCompile(`^[a-z0-9]{6,32}$`)

// State is what a session remembers.
type State struct {
	ID            string    `json:"id"`
	PID           int       `json:"pid,omitempty"`
	ClusterID     string    `json:"clusterID,omitempty"`
	ClusterName   string    `json:"clusterName,omitempty"`
	Namespace     string    `json:"namespace,omitempty"`
	ReadOnly      bool      `json:"readOnly,omitempty"`
	UnlockedUntil time.Time `json:"unlockedUntil,omitempty"`
	// ProxyURL is set when the session goes through tugd's API proxy.
	ProxyURL string    `json:"proxyURL,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// Connected reports whether the session points at a cluster.
func (s *State) Connected() bool { return s.ClusterID != "" }

// Writable reports whether writes are allowed at time now.
func (s *State) Writable(now time.Time) bool {
	return !s.ReadOnly || now.Before(s.UnlockedUntil)
}

// Manager creates and finds sessions under one runtime directory.
type Manager struct {
	root string
	now  func() time.Time
}

// NewManager returns a Manager for runtimeDir.
func NewManager(runtimeDir string) *Manager {
	return &Manager{root: filepath.Join(runtimeDir, "sessions"), now: time.Now}
}

// Root is the directory that holds every session.
func (m *Manager) Root() string { return m.root }

// Session is one terminal's state on disk.
type Session struct {
	State State
	dir   string
	m     *Manager
}

// Dir is the session directory.
func (s *Session) Dir() string { return s.dir }

// KubeconfigPath is the file KUBECONFIG points at while connected.
func (s *Session) KubeconfigPath() string { return filepath.Join(s.dir, kubeconfigFileName) }

// Create makes a new session owned by pid. When parentID names an existing
// session, the new one starts connected to the same cluster and namespace, so
// a nested shell begins where its parent was but has its own state.
func (m *Manager) Create(pid int, parentID string) (*Session, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := m.now()
	s := &Session{
		State: State{ID: id, PID: pid, Created: now, Updated: now},
		dir:   filepath.Join(m.root, id),
		m:     m,
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating session directory: %w", err)
	}
	if parentID != "" {
		if parent, err := m.Open(parentID); err == nil && parent.State.Connected() {
			if err := s.copyConnection(parent); err != nil {
				return nil, err
			}
		}
	}
	if err := s.Save(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Session) copyConnection(parent *Session) error {
	s.State.ClusterID = parent.State.ClusterID
	s.State.ClusterName = parent.State.ClusterName
	s.State.Namespace = parent.State.Namespace
	s.State.ReadOnly = parent.State.ReadOnly
	for _, f := range []string{kubeconfigFileName, promptFileName} {
		b, err := os.ReadFile(filepath.Join(parent.dir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := config.WriteFileAtomic(filepath.Join(s.dir, f), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Open loads a session by ID.
func (m *Manager) Open(id string) (*Session, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	dir := filepath.Join(m.root, id)
	b, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: session %s has ended", ErrNoSession, id)
	}
	if err != nil {
		return nil, err
	}
	s := &Session{dir: dir, m: m}
	if err := json.Unmarshal(b, &s.State); err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	return s, nil
}

// Current returns the session named by $TUG_SESSION.
func (m *Manager) Current() (*Session, error) {
	id := os.Getenv(EnvSession)
	if id == "" {
		return nil, ErrNoSession
	}
	return m.Open(id)
}

// Remove deletes a session directory.
func (m *Manager) Remove(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	return os.RemoveAll(filepath.Join(m.root, id))
}

// Sweep removes sessions whose shell has exited, and any session not updated
// within maxAge. It returns how many it removed.
func (m *Manager) Sweep(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(m.root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !validID.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		s, err := m.Open(e.Name())
		old := m.now().Sub(info.ModTime()) > maxAge
		dead := err == nil && s.State.PID > 0 && !processAlive(s.State.PID)
		orphan := err != nil && m.now().Sub(info.ModTime()) > time.Hour
		if old || dead || orphan {
			if os.RemoveAll(filepath.Join(m.root, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// Save writes the session state.
func (s *Session) Save() error {
	s.State.Updated = s.m.now()
	b, err := json.MarshalIndent(s.State, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(s.dir, stateFileName), b, 0o600)
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
