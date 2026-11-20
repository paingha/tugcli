package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Errors about tugd's state.
var (
	// ErrNotRunning is returned by Client when tugd is not running.
	ErrNotRunning = errors.New("tugd is not running")
	// ErrAlreadyRunning is returned by Run when another tugd answers on the
	// socket.
	ErrAlreadyRunning = errors.New("tugd is already running")
)

// Status is tugd's answer to a status request.
type Status struct {
	PID      int       `json:"pid"`
	Version  string    `json:"version"`
	ProxyURL string    `json:"proxyURL"`
	Started  time.Time `json:"started"`
	Sessions int       `json:"sessions"`
}

// Registration is a session's access to the proxy.
type Registration struct {
	Token    string `json:"token"`
	ProxyURL string `json:"proxyURL"`
	// ProxyCA is the PEM certificate the session's kubeconfig trusts.
	ProxyCA []byte `json:"proxyCA"`
}

// ControlHandler serves the control API on the Unix socket. Only the user
// can reach the socket, so it needs no further authentication.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		st := Status{PID: os.Getpid(), Version: s.cfg.Version, ProxyURL: s.proxyURL, Started: s.started, Sessions: len(s.tokens)}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("POST /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		tok, err := s.Register(r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, Registration{Token: tok, ProxyURL: s.ProxyURL(), ProxyCA: s.ProxyCA()})
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
		s.Shutdown()
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Client talks to a running tugd over its control socket.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the tugd of a runtime directory.
func NewClient(runtimeDir string) *Client {
	sock := SocketPath(runtimeDir)
	return &Client{http: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}}
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://tugd"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return fmt.Errorf("%w: %w", ErrNotRunning, err)
		}
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("tugd answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// Status returns the running tugd's status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.do(ctx, http.MethodGet, "/v1/status", &st)
	return st, err
}

// Register returns a session's proxy token and the proxy's address.
func (c *Client) Register(ctx context.Context, sessionID string) (Registration, error) {
	var reg Registration
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+sessionID, &reg)
	return reg, err
}

// Shutdown stops tugd.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/shutdown", nil)
}
