package iap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// stallingTokens gives out tokens until it has given limit, then makes
// every later call hang until release is closed.
type stallingTokens struct {
	mu      sync.Mutex
	calls   int
	limit   int
	release chan struct{}
}

func newStallingTokens(t *testing.T, limit int) *stallingTokens {
	s := &stallingTokens{limit: limit, release: make(chan struct{})}
	t.Cleanup(func() { close(s.release) })
	return s
}

func (s *stallingTokens) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	s.calls++
	hang := s.calls > s.limit
	s.mu.Unlock()
	if hang {
		<-s.release
	}
	return &oauth2.Token{AccessToken: testToken}, nil
}

func (s *stallingTokens) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestDialStopsWaitingForAToken(t *testing.T) {
	r := newFakeRelay(t)
	tokens := newStallingTokens(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	dialed := make(chan error, 1)
	go func() {
		_, err := Dial(ctx, instanceConfig, "bastion:22", r.options(WithTokenSource(tokens))...)
		dialed <- err
	}()
	select {
	case err := <-dialed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Dial with a hanging token source = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dial still waits for a token after its context ended")
	}
}

func TestCloseWhileTokenRefreshHangs(t *testing.T) {
	r := newFakeRelay(t)
	tokens := newStallingTokens(t, 1)
	c := dial(t, r, WithTokenSource(tokens))
	s := r.session()
	r.set(func() { s.dropAt = 1 }) // cut the connection, so the tunnel resumes
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitFor(t, "the resume to ask for a token", func() bool { return tokens.count() > 1 })

	start := time.Now()
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hangs while the token refresh hangs")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Close took %s", took)
	}
	if !isDone(c.done) {
		t.Error("the session goroutine is still running after Close")
	}
}

// TestDefaultTokenSourceOutlivesItsContext checks that a shared token
// source keeps refreshing after the context it was made with ends, as when
// the first tunnel's dial is over.
func TestDefaultTokenSourceOutlivesItsContext(t *testing.T) {
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("refresh_token") != "test-refresh-token" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		n := refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// A token that is already due for refreshing.
		fmt.Fprintf(w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":1}`, n)
	}))
	defer srv.Close()

	// The credentials `gcloud auth application-default login` writes, whose
	// refreshes go out with the source's context.
	creds, err := json.Marshal(map[string]string{
		"type":          "authorized_user",
		"client_id":     "test-client",
		"client_secret": "test-secret",
		"refresh_token": "test-refresh-token",
		"token_uri":     srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, creds, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)

	ctx, cancel := context.WithCancel(context.Background())
	ts, err := DefaultTokenSource(ctx)
	if err != nil {
		t.Fatalf("DefaultTokenSource: %v", err)
	}
	cancel()
	for i := range 2 {
		tok, err := ts.Token()
		if err != nil {
			t.Fatalf("Token() after the context ended: %v", err)
		}
		if want := fmt.Sprintf("token-%d", i+1); tok.AccessToken != want {
			t.Errorf("Token() = %s, want %s", tok.AccessToken, want)
		}
	}
}
