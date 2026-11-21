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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"k8s.io/client-go/tools/clientcmd"

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
	lockFileName       = "lock"
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
	// Env is the cluster's env label, which the prompt shows.
	Env string `json:"env,omitempty"`
	// Production is set when the cluster's labels make it production (by
	// default env=prod), whatever the session's mode, so the prompt marks
	// it even on a session opened read-write.
	Production bool `json:"production,omitempty"`
	// ProxyURL is set when the session goes through tugd's API proxy.
	ProxyURL string `json:"proxyURL,omitempty"`
	// Profile is the profile the session was connected with, until it
	// connects some other way.
	Profile string `json:"profile,omitempty"`
	// Pin is set while a .tug.yaml chose the connection.
	Pin *Pin `json:"pin,omitempty"`
	// PinNoticed is the pin file, path and hash, the user was last told
	// is not allowed, so moving around its tree does not repeat it.
	PinNoticed string    `json:"pinNoticed,omitempty"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
}

// Pin records a connection a .tug.yaml made, and the one to go back to
// when the shell leaves the file's directory.
type Pin struct {
	File string `json:"file"`
	Hash string `json:"hash"`
	// Previous is the connection before the pin; nil when there was none.
	Previous *Connection `json:"previous,omitempty"`
}

// Connection is enough of a session's state to connect it again.
type Connection struct {
	ClusterID string `json:"clusterID"`
	Namespace string `json:"namespace,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
	Proxy     bool   `json:"proxy,omitempty"`
	Profile   string `json:"profile,omitempty"`
}

// Current returns the session's connection, or nil when it has none.
func (s *State) Current() *Connection {
	if !s.Connected() {
		return nil
	}
	return &Connection{ClusterID: s.ClusterID, Namespace: s.Namespace, ReadOnly: s.ReadOnly, Proxy: s.ProxyURL != "", Profile: s.Profile}
}

// Connected reports whether the session points at a cluster.
func (s *State) Connected() bool { return s.ClusterID != "" }

// Writable reports whether writes are allowed at time now.
func (s *State) Writable(now time.Time) bool {
	return !s.ReadOnly || now.Before(s.UnlockedUntil)
}

// Manager creates and finds sessions under one runtime directory.
type Manager struct {
	runtime    string
	root       string
	now        func() time.Time
	production func(labels map[string]string) bool
}

// Option changes a Manager.
type Option func(*Manager)

// WithProduction sets how Connect tells a production cluster from its
// labels. By default a cluster labelled env=prod is production.
func WithProduction(f func(labels map[string]string) bool) Option {
	return func(m *Manager) { m.production = f }
}

// NewManager returns a Manager for runtimeDir.
func NewManager(runtimeDir string, opts ...Option) *Manager {
	m := &Manager{
		runtime:    runtimeDir,
		root:       filepath.Join(runtimeDir, "sessions"),
		now:        time.Now,
		production: func(labels map[string]string) bool { return labels["env"] == "prod" },
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// checkPrivate refuses a runtime directory that other users can change (see
// config.CheckPrivateDir). A session kubeconfig's exec entry names a command
// that kubectl runs, so another user who could replace the file would run
// their own program as this user. A directory that does not exist yet is
// fine: Create makes it private.
func (m *Manager) checkPrivate() error {
	if err := config.CheckPrivateDir(m.runtime); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
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
	if err := config.EnsurePrivateDir(m.runtime); err != nil {
		return nil, err
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

// copyConnection starts the session where its parent is. A proxied parent's
// kubeconfig names the parent's session in its server address and exec
// entry, so the copy is pointed at this session instead: the child's own
// guard state and lock/unlock then govern its clients.
func (s *Session) copyConnection(parent *Session) error {
	s.State.ClusterID = parent.State.ClusterID
	s.State.ClusterName = parent.State.ClusterName
	s.State.Namespace = parent.State.Namespace
	s.State.ReadOnly = parent.State.ReadOnly
	s.State.Env = parent.State.Env
	s.State.Production = parent.State.Production
	s.State.Profile = parent.State.Profile
	s.State.ProxyURL = parent.State.ProxyURL
	if s.State.ProxyURL != "" {
		cfg, err := clientcmd.LoadFromFile(parent.KubeconfigPath())
		if err != nil {
			return fmt.Errorf("reading the parent session's kubeconfig: %w", err)
		}
		var ca []byte
		for _, cl := range cfg.Clusters {
			ca = cl.CertificateAuthorityData
		}
		repoint(cfg, s.State.ProxyURL, ca, s.State.ID)
		if err := writeKubeconfig(s.KubeconfigPath(), cfg); err != nil {
			return err
		}
	}
	for _, f := range []string{kubeconfigFileName, promptFileName} {
		if f == kubeconfigFileName && s.State.ProxyURL != "" {
			continue // written above
		}
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
	if err := m.checkPrivate(); err != nil {
		return nil, err
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
	if err := m.checkPrivate(); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(m.root, id))
}

// Sweep removes sessions whose shell has exited, and any session not updated
// within maxAge. It returns the state of each session it removed; one whose
// state could not be read has only its ID.
func (m *Manager) Sweep(maxAge time.Duration) ([]State, error) {
	if err := m.checkPrivate(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(m.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []State
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
				st := State{ID: e.Name()}
				if err == nil {
					st = s.State
				}
				removed = append(removed, st)
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
