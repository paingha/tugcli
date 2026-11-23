// Package daemon is tugd: a small background process that owns what must
// outlive a single tug command. It serves a local Kubernetes API proxy for
// sessions that opt in, so read-only sessions are enforced for every tool
// (kubectl, helm, k9s) and not only for tug's own commands. Its control API
// listens on a Unix socket in tug's private runtime directory.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/audit"
	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/filelock"
	"github.com/paingha/tugcli/internal/session"
)

// Defaults for Config.
const (
	DefaultIdleTimeout = 30 * time.Minute
	// upstreamTTL bounds how long a cluster's proxy is reused, so catalog
	// changes (a new CA, a moved endpoint) are picked up.
	upstreamTTL = time.Minute
	// wakeGap is how far the wall clock may run ahead of the monotonic
	// clock between two ticks before tugd takes it that the machine slept.
	wakeGap = 10 * time.Second
)

// Config is what tugd needs from its host.
type Config struct {
	// RuntimeDir is tug's private runtime directory.
	RuntimeDir string
	// Sessions reads session state. The guard checks it on every request, so
	// tug unlock and tug lock take effect at once.
	Sessions *session.Manager
	// Upstream returns a client config, with credentials, for a catalog
	// cluster. tugd applies the guard itself. With refresh set, the cluster
	// refused the last credential, so a new one is asked of its provider
	// rather than taken from the cache.
	Upstream func(ctx context.Context, clusterID string, refresh bool) (*rest.Config, error)
	// ProxyAddr is the proxy's listen address; by default a loopback port,
	// reusing the previous one when it is free.
	ProxyAddr string
	// IdleTimeout ends tugd once no live session has used it for this long
	// and no named port-forward is running.
	IdleTimeout time.Duration
	// Forward runs named port-forwards; without it tugd refuses them.
	Forward ForwardFunc
	// Audit, when set, records every write the proxy forwards and how it
	// was answered, every write it refuses, and unlocks that run out. A
	// write that cannot be recorded is not forwarded.
	Audit Recorder
	// SuggestedUnlock, when set, says how long the refusal of a write on a
	// read-only session suggests unlocking for, such as a team policy's
	// limit, so that tug unlock does not refuse what the hint suggests.
	// Without it, or when it returns zero, the refusal suggests
	// guard.DefaultUnlock.
	SuggestedUnlock func(st session.State) time.Duration
	Version         string
	Now             func() time.Time
	Log             *slog.Logger
}

// Recorder notes what the proxy forwards and refuses in the audit log.
type Recorder interface {
	Record(ctx context.Context, e audit.Entry) error
}

// Server is a running tugd.
type Server struct {
	cfg Config

	mu        sync.Mutex
	tokens    map[string]string // session ID to its proxy token
	upstreams map[string]*upstream
	building  map[string]*sync.Mutex // per cluster, held while its upstream is built
	refresh   map[string]bool        // clusters whose next upstream needs a new credential
	forwards  map[string]*forwardRunner
	// probe checks that a local port is free before a group of forwards
	// asks for it.
	probe      func(addresses []string, port uint16) error
	proxyURL   string
	proxyCA    []byte
	lastActive time.Time
	started    time.Time

	shutdownOnce sync.Once
	shutdown     chan struct{}
}

// New returns a Server.
func New(cfg Config) *Server {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Server{
		cfg:       cfg,
		tokens:    map[string]string{},
		upstreams: map[string]*upstream{},
		building:  map[string]*sync.Mutex{},
		refresh:   map[string]bool{},
		forwards:  map[string]*forwardRunner{},
		probe:     listenProbe,
		shutdown:  make(chan struct{}),
	}
}

// SocketPath is the control socket. Unix socket paths are limited to about
// 104 bytes, so a long runtime directory falls back to a short directory of
// its own in the system temp directory, named after the runtime directory.
// tugd and its clients only use the socket's directory when it is private to
// the user (see config.CheckPrivateDir), so another user cannot create it
// first and answer in tugd's place.
func SocketPath(runtimeDir string) string {
	p := filepath.Join(runtimeDir, "tugd.sock")
	if len(p) < 100 {
		return p
	}
	return filepath.Join(os.TempDir(), "tugd-"+shortHash(runtimeDir), "tugd.sock")
}

func shortHash(s string) string {
	var h uint64 = 14695981039346656037 // FNV-1a
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 36)
}

// stateFile records tugd's last proxy address. It stays after tugd stops, so
// the next tugd listens on the same port and session kubeconfigs keep working.
func stateFile(runtimeDir string) string { return filepath.Join(runtimeDir, "tugd.json") }

// lockFile is held by the running tugd for its whole life.
func lockFile(runtimeDir string) string { return filepath.Join(runtimeDir, "tugd.lock") }

// State is what tugd records about itself when it starts. Whether tugd is
// running is told by its control socket, not by this file.
type State struct {
	PID      int       `json:"pid"`
	Version  string    `json:"version"`
	ProxyURL string    `json:"proxyURL"`
	Started  time.Time `json:"started"`
}

func readState(runtimeDir string) (State, error) {
	var st State
	b, err := os.ReadFile(stateFile(runtimeDir))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// Register returns the proxy token for a session, creating it on first
// use. Tokens live only in tugd's memory.
func (s *Server) Register(sessionID string) (string, error) {
	if _, err := s.cfg.Sessions.Open(sessionID); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = s.cfg.Now()
	if tok, ok := s.tokens[sessionID]; ok {
		return tok, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	s.tokens[sessionID] = tok
	return tok, nil
}

// validToken compares a presented token with the session's in constant time.
func (s *Server) validToken(sessionID, presented string) bool {
	s.mu.Lock()
	want, ok := s.tokens[sessionID]
	if ok {
		s.lastActive = s.cfg.Now()
	}
	s.mu.Unlock()
	return ok && presented != "" && subtle.ConstantTimeCompare([]byte(want), []byte(presented)) == 1
}

// ProxyURL is the proxy's base URL once Run has started listening.
func (s *Server) ProxyURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyURL
}

// ProxyCA is the PEM certificate clients trust for the proxy.
func (s *Server) ProxyCA() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyCA
}

// Shutdown asks Run to stop.
func (s *Server) Shutdown() { s.shutdownOnce.Do(func() { close(s.shutdown) }) }

// usesProxy reports whether a session still goes through tugd: it exists, is
// connected, and was connected with --proxy.
func (s *Server) usesProxy(sessionID string) (*session.Session, bool) {
	sess, err := s.cfg.Sessions.Open(sessionID)
	if err != nil || !sess.State.Connected() || sess.State.ProxyURL == "" {
		return nil, false
	}
	return sess, true
}

// suggestedUnlock is how long the refusal of a write on st suggests
// unlocking for; zero leaves it to guard.DeniedError's default.
func (s *Server) suggestedUnlock(st session.State) time.Duration {
	if s.cfg.SuggestedUnlock == nil {
		return 0
	}
	return s.cfg.SuggestedUnlock(st)
}

// idle reports whether no live session has used tugd for IdleTimeout. It
// forgets the tokens of sessions that no longer use the proxy: ended,
// disconnected, or reconnected without --proxy.
func (s *Server) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.tokens {
		if _, ok := s.usesProxy(id); !ok {
			delete(s.tokens, id)
		}
	}
	return len(s.tokens) == 0 && len(s.forwards) == 0 && s.cfg.Now().Sub(s.lastActive) > s.cfg.IdleTimeout
}

// slept reports whether the machine seems to have slept between two
// readings of the clock, prev and now: the wall clock, which counts time
// asleep, ran ahead of the monotonic clock, which on most systems does not,
// by more than wakeGap. A clock set forwards looks the same, which costs
// only a few new connections.
func slept(prev, now time.Time) bool {
	return sleptFor(now.Round(0).Sub(prev.Round(0)), now.Sub(prev))
}

// sleptFor is slept for the time that passed on the wall clock and on the
// monotonic clock.
func sleptFor(wall, monotonic time.Duration) bool { return wall-monotonic > wakeGap }

// endExpiredUnlocks records the unlocks of proxied sessions that have run
// out, so the audit log has them even when no request comes after.
func (s *Server) endExpiredUnlocks(ctx context.Context) {
	s.mu.Lock()
	ids := slices.Collect(maps.Keys(s.tokens))
	s.mu.Unlock()
	now := s.cfg.Now()
	for _, id := range ids {
		if sess, ok := s.usesProxy(id); ok {
			s.endExpiredUnlock(ctx, sess, now)
		}
	}
}

// Run listens on the control socket and the proxy address and serves until
// ctx ends, Shutdown is called, or tugd has been idle for IdleTimeout.
func (s *Server) Run(ctx context.Context) error {
	if err := config.EnsurePrivateDir(s.cfg.RuntimeDir); err != nil {
		return err
	}
	// The lock is held until Run returns. Two tugds started at once take
	// turns here, so the second never removes the first one's socket, and
	// only the holder cleans up.
	unlock, err := filelock.TryLock(lockFile(s.cfg.RuntimeDir))
	if errors.Is(err, filelock.ErrLocked) {
		return ErrAlreadyRunning
	}
	if err != nil {
		return fmt.Errorf("locking %s: %w", lockFile(s.cfg.RuntimeDir), err)
	}
	defer unlock()

	sock := SocketPath(s.cfg.RuntimeDir)
	if dir := filepath.Dir(sock); dir != filepath.Clean(s.cfg.RuntimeDir) {
		if err := config.EnsurePrivateDir(dir); err != nil {
			return err
		}
		defer os.Remove(dir)
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	ctl, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", sock, err)
	}
	defer os.Remove(sock)
	if err := os.Chmod(sock, 0o600); err != nil {
		ctl.Close()
		return err
	}

	now := s.cfg.Now()
	cert, caPEM, err := loadOrCreateTLS(s.cfg.RuntimeDir, now)
	if err != nil {
		ctl.Close()
		return fmt.Errorf("preparing the proxy certificate: %w", err)
	}
	pl, err := s.listenProxy()
	if err != nil {
		ctl.Close()
		return err
	}
	pl = tls.NewListener(pl, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	s.mu.Lock()
	s.proxyURL = "https://" + pl.Addr().String()
	s.proxyCA = caPEM
	s.started, s.lastActive = now, now
	s.mu.Unlock()

	st := State{PID: os.Getpid(), Version: s.cfg.Version, ProxyURL: s.ProxyURL(), Started: now}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	// Kept after tugd stops: the next tugd reads it to listen on this port.
	if err := config.WriteFileAtomic(stateFile(s.cfg.RuntimeDir), b, 0o600); err != nil {
		return err
	}

	ctlSrv := &http.Server{Handler: s.ControlHandler(), ReadHeaderTimeout: 10 * time.Second}
	// No write timeout on the proxy: watches, logs -f and exec stream.
	proxySrv := &http.Server{Handler: s.ProxyHandler(), ReadHeaderTimeout: 30 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- ctlSrv.Serve(ctl) }()
	go func() { errs <- proxySrv.Serve(pl) }()
	s.cfg.Log.Info("tugd started", "proxy", s.ProxyURL(), "socket", sock)

	tick := time.NewTicker(min(s.cfg.IdleTimeout/2, 30*time.Second))
	defer tick.Stop()
	lastTick := time.Now()
	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-s.shutdown:
			break loop
		case <-tick.C:
			now := time.Now()
			if slept(lastTick, now) {
				s.cfg.Log.Info("the machine woke from sleep; closing idle connections to clusters")
				s.dropUpstreams()
			}
			lastTick = now
			s.endExpiredUnlocks(ctx)
			if s.idle() {
				s.cfg.Log.Info("tugd idle; stopping")
				break loop
			}
		case err := <-errs:
			if !errors.Is(err, http.ErrServerClosed) {
				runErr = err
			}
			break loop
		}
	}
	s.stopForwards()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ctlSrv.Shutdown(sctx)
	if err := proxySrv.Shutdown(sctx); err != nil {
		_ = proxySrv.Close() // streams and upgraded connections
	}
	return runErr
}

// listenProxy listens on the configured address, or on the previous port
// so existing session kubeconfigs keep working, or on any free port.
func (s *Server) listenProxy() (net.Listener, error) {
	if s.cfg.ProxyAddr != "" {
		return net.Listen("tcp", s.cfg.ProxyAddr)
	}
	if prev, err := readState(s.cfg.RuntimeDir); err == nil && prev.ProxyURL != "" {
		if l, err := net.Listen("tcp", strings.TrimPrefix(prev.ProxyURL, "https://")); err == nil {
			return l, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}
