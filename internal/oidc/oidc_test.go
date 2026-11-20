package oidc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/oidc/oidctest"
)

// clock is a settable clock shared by a client and its issuer.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	iss   *oidctest.Issuer
	store *credentials.MemoryStore
	clock *clock
	cfg   Config
	told  []string
	mu    sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{iss: oidctest.NewIssuer(t), store: credentials.NewMemoryStore(), clock: &clock{t: time.Now()}}
	f.iss.Set(func(iss *oidctest.Issuer) { iss.Now = f.clock.now })
	f.cfg = Config{Issuer: f.iss.URL, ClientID: oidctest.ClientID, Scopes: []string{"groups"}}
	return f
}

// ui is a sign-in UI that records what it tells the user and "opens" the
// browser by following the address.
func (f *fixture) ui() UI {
	return UI{
		Tell: func(msg string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.told = append(f.told, msg)
		},
		OpenBrowser: f.iss.Browser(),
	}
}

func (f *fixture) client(opts ...Option) *Client {
	base := []Option{WithNow(f.clock.now), WithListenAddresses("127.0.0.1:0"), WithLoginTimeout(10 * time.Second)}
	return New(f.store, append(base, opts...)...)
}

func TestTokenSignsInRefreshesAndReuses(t *testing.T) {
	f := newFixture(t)
	c := f.client(WithLogin(f.ui()))
	ctx := context.Background()

	tok, err := c.Token(ctx, f.cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Identity != oidctest.Email || tok.IDToken == "" || !tok.Expiry.After(f.clock.now()) {
		t.Fatalf("Token = %+v", tok)
	}
	if logins, _ := f.iss.Counts(); logins != 1 || len(f.iss.Challenges()) != 1 || f.iss.Challenges()[0] == "" {
		t.Fatalf("logins %d, PKCE challenges %q; want one sign-in with PKCE", logins, f.iss.Challenges())
	}
	if len(f.told) == 0 || !strings.Contains(f.told[0], f.iss.URL+"/authorize?") {
		t.Fatalf("the user was not shown the sign-in address: %q", f.told)
	}

	// A valid ID token is reused without asking the issuer.
	if again, err := c.Token(ctx, f.cfg, false); err != nil || again.IDToken != tok.IDToken {
		t.Fatalf("second Token = %+v, %v; want the stored token", again, err)
	}

	// An expired one is refreshed, with no new sign-in, and the rotated
	// refresh token is kept for next time.
	f.clock.advance(2 * time.Hour)
	for want := 1; want <= 2; want++ {
		refreshed, err := c.Token(ctx, f.cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		if logins, refreshes := f.iss.Counts(); refreshed.IDToken == tok.IDToken || refreshes != want || logins != 1 {
			t.Fatalf("after expiry: refreshes %d, logins %d; want %d and 1", refreshes, logins, want)
		}
		tok = refreshed
		f.clock.advance(2 * time.Hour)
	}

	// refresh skips the stored ID token.
	if _, err := c.Token(ctx, f.cfg, true); err != nil {
		t.Fatal(err)
	}
	if _, refreshes := f.iss.Counts(); refreshes != 3 {
		t.Fatalf("Token(refresh) made %d refreshes in all, want 3", refreshes)
	}
}

func TestTokenWithoutLoginNeedsASignIn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.client().Token(ctx, f.cfg, false); !errors.Is(err, credentials.ErrLoginRequired) {
		t.Fatalf("Token with nothing stored = %v, want ErrLoginRequired", err)
	}

	// A rejected refresh token is forgotten, and a sign-in is needed again.
	if _, err := f.client().Login(ctx, f.cfg, f.ui()); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(2 * time.Hour)
	f.iss.Set(func(iss *oidctest.Issuer) { iss.RejectRefresh = true })
	if _, err := f.client().Token(ctx, f.cfg, false); !errors.Is(err, credentials.ErrLoginRequired) {
		t.Fatalf("Token with a rejected refresh token = %v, want ErrLoginRequired", err)
	}
	if had, _ := f.client().Logout(f.cfg); had {
		t.Fatal("the rejected sign-in was kept")
	}
}

func TestLoginAndLogout(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.client()
	first, err := c.Login(ctx, f.cfg, f.ui())
	if err != nil {
		t.Fatal(err)
	}
	// Login signs in again even with a valid token.
	second, err := c.Login(ctx, f.cfg, f.ui())
	if logins, _ := f.iss.Counts(); err != nil || second.IDToken == first.IDToken || logins != 2 {
		t.Fatalf("second Login = %v, logins %d; want a new sign-in", err, logins)
	}
	// Scopes are part of the sign-in: openid is implied, order does not matter.
	same := Config{Issuer: f.iss.URL + "/", ClientID: oidctest.ClientID, Scopes: []string{"groups", "openid"}}
	if _, err := c.Token(ctx, same, false); err != nil {
		t.Fatalf("Token with the same scopes reordered = %v, want the stored sign-in", err)
	}

	if had, err := c.Logout(f.cfg); err != nil || !had {
		t.Fatalf("Logout = %v, %v; want true", had, err)
	}
	if had, err := c.Logout(f.cfg); err != nil || had {
		t.Fatalf("second Logout = %v, %v; want false", had, err)
	}
	if _, err := c.Token(ctx, f.cfg, false); !errors.Is(err, credentials.ErrLoginRequired) {
		t.Fatalf("Token after Logout = %v, want ErrLoginRequired", err)
	}
}

func TestDeviceLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the device poll interval")
	}
	f := newFixture(t)
	ui := f.ui()
	ui.Device = true
	ui.OpenBrowser = nil
	tok, err := f.client().Login(context.Background(), f.cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Identity != oidctest.Email || len(f.told) != 1 || !strings.Contains(f.told[0], "ABCD-EFGH") {
		t.Fatalf("device Login = %+v, told %q", tok, f.told)
	}
}

func TestDeviceFallsBackToTheBrowser(t *testing.T) {
	f := newFixture(t)
	f.iss.Set(func(iss *oidctest.Issuer) { iss.NoDevice = true })
	ui := f.ui()
	ui.Device = true
	if _, err := f.client().Login(context.Background(), f.cfg, ui); err != nil {
		t.Fatal(err)
	}
	if len(f.told) < 2 || !strings.Contains(f.told[0], "does not offer device sign-in") || !strings.Contains(f.told[1], "/authorize?") {
		t.Fatalf("told %q; want the fallback explained, then the browser sign-in", f.told)
	}
}

func TestLoginRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture)
		ui    func(f *fixture) UI
		want  string
	}{
		{"wrong state", func(f *fixture) { f.iss.Set(func(iss *oidctest.Issuer) { iss.WrongState = true }) }, (*fixture).ui, "wrong state"},
		{"wrong nonce", func(f *fixture) { f.iss.Set(func(iss *oidctest.Issuer) { iss.WrongNonce = true }) }, (*fixture).ui, "nonce mismatch"},
		{"plain http issuer", func(f *fixture) { f.cfg.Issuer = "http://idp.example.com" }, (*fixture).ui, "must use https"},
		{"another issuer", func(f *fixture) { f.cfg.Issuer = f.iss.URL + "/realms/other" }, (*fixture).ui, "answered 404"},
		{"nobody signs in", func(*fixture) {}, func(f *fixture) UI {
			ui := f.ui()
			ui.OpenBrowser = nil
			return ui
		}, "took longer than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(f)
			c := f.client(WithLoginTimeout(500 * time.Millisecond))
			_, err := c.Login(context.Background(), f.cfg, tt.ui(f))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Login = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestWithoutPKCE(t *testing.T) {
	f := newFixture(t)
	f.iss.Set(func(iss *oidctest.Issuer) { iss.NoPKCE = true })
	if _, err := f.client().Login(context.Background(), f.cfg, f.ui()); err != nil {
		t.Fatal(err)
	}
	if got := f.iss.Challenges(); len(got) != 1 || got[0] != "" {
		t.Fatalf("challenges %q; want none when the issuer does not advertise S256", got)
	}
}

// TestConcurrentTokensSignInOnce stands for several kubectl calls that need
// the same sign-in at once: the lock lets one sign in and the rest reuse it.
func TestConcurrentTokensSignInOnce(t *testing.T) {
	f := newFixture(t)
	lockDir := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = f.client(WithLockDir(lockDir), WithLogin(f.ui())).Token(context.Background(), f.cfg, false)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if logins, _ := f.iss.Counts(); logins != 1 {
		t.Fatalf("%d sign-ins, want 1", logins)
	}
}

func TestParseToken(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	tests := []struct {
		name, raw, want string
	}{
		{"not a JWT", "abc", "not a JWT"},
		{"bad payload", "a.!!!.c", "decoding the ID token"},
		{"no expiry", "e30.e30.sig", "no expiry"}, // {} and {}
	}
	for _, tt := range tests {
		if _, err := parseToken(tt.raw); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: parseToken = %v, want %q", tt.name, err, tt.want)
		}
	}
	tok, err := parseToken(iss.IDToken("n"))
	if err != nil || tok.Identity != oidctest.Email {
		t.Errorf("parseToken(valid) = %+v, %v", tok, err)
	}
}
