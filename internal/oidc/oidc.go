// Package oidc signs in to OpenID Connect issuers for clusters whose API
// servers accept ID tokens, and keeps those tokens fresh. Sign-in uses the
// authorization code flow with PKCE through a loopback redirect, or the
// device authorization grant where there is no browser. Tokens live only in
// the credential store: the refresh token, and the ID token while it is
// valid.
package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/filelock"
	"github.com/paingha/tugcli/pkg/provider"
)

// DefaultListenAddresses are where tug waits for the browser to come back
// after sign-in, tried in order, with http://localhost:<port> as the
// redirect URI. They are kubelogin's defaults, so an issuer that already
// accepts http://localhost:8000 and http://localhost:18000 works with tug.
var DefaultListenAddresses = []string{"127.0.0.1:8000", "127.0.0.1:18000"}

// DefaultLoginTimeout is how long a sign-in may wait for the user.
const DefaultLoginTimeout = 5 * time.Minute

// Config names an issuer and the client that signs in to it. It comes from a
// provider's oidc credential.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// ConfigFor returns the sign-in settings of a credential of type oidc.
func ConfigFor(c provider.Credential) Config {
	return Config{Issuer: c.Issuer, ClientID: c.ClientID, ClientSecret: c.ClientSecret, Scopes: c.Scopes}
}

// normalised trims the issuer's trailing slash and makes sure the scopes
// start with openid, without repeats.
func (c Config) normalised() Config {
	c.Issuer = strings.TrimRight(c.Issuer, "/")
	scopes := []string{"openid"}
	for _, s := range c.Scopes {
		if s != "" && !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	c.Scopes = scopes
	return c
}

// id names the sign-in in the credential store. Clusters that share an
// issuer, client and scopes share one sign-in.
func (c Config) id() string {
	scopes := slices.Clone(c.Scopes)
	slices.Sort(scopes)
	sum := sha256.Sum256([]byte(c.Issuer + "\x00" + c.ClientID + "\x00" + strings.Join(scopes, " ")))
	return hex.EncodeToString(sum[:16])
}

// Token is an ID token and when it expires.
type Token struct {
	IDToken string
	Expiry  time.Time
	// Identity is who signed in: the email claim, else the subject.
	Identity string
}

// UI is how a sign-in reaches the user.
type UI struct {
	// Tell shows the user what to do, such as the address to open or the
	// code to enter. It must not be nil.
	Tell func(msg string)
	// OpenBrowser opens an address in a browser. When it is nil, or fails,
	// the user opens the address that Tell showed.
	OpenBrowser func(ctx context.Context, url string) error
	// Device prefers the device authorization grant to a browser on this
	// machine, for example over SSH. Issuers that do not offer it get the
	// browser sign-in.
	Device bool
}

// Client signs in to issuers and keeps their tokens in a credential store.
type Client struct {
	store   credentials.Store
	http    *http.Client
	now     func() time.Time
	log     *slog.Logger
	lockDir string
	listen  []string
	timeout time.Duration
	// ui, when set, lets Token sign in on its own.
	ui *UI
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the client for requests to issuers.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithNow sets the clock.
func WithNow(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// WithLogger sets where the client logs.
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.log = l } }

// WithLockDir keeps lock files in dir, so tug processes that need the same
// sign-in take turns instead of each opening a browser.
func WithLockDir(dir string) Option { return func(c *Client) { c.lockDir = dir } }

// WithListenAddresses sets the loopback addresses tried, in order, for the
// sign-in redirect. Empty keeps DefaultListenAddresses.
func WithListenAddresses(addrs ...string) Option {
	return func(c *Client) {
		if len(addrs) > 0 {
			c.listen = addrs
		}
	}
}

// WithLoginTimeout sets how long a sign-in may wait for the user.
func WithLoginTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithLogin lets Token sign in when it has no token it can use or refresh.
// Without it, Token returns an error wrapping credentials.ErrLoginRequired.
func WithLogin(ui UI) Option { return func(c *Client) { c.ui = &ui } }

// New returns a client that keeps tokens in store.
func New(store credentials.Store, opts ...Option) *Client {
	c := &Client{
		store:   store,
		http:    &http.Client{Timeout: 30 * time.Second},
		now:     time.Now,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		listen:  DefaultListenAddresses,
		timeout: DefaultLoginTimeout,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ credentials.OIDCTokens = (*Client)(nil)

// IDToken implements credentials.OIDCTokens.
func (c *Client) IDToken(ctx context.Context, cred provider.Credential, refresh bool) (string, time.Time, error) {
	t, err := c.Token(ctx, ConfigFor(cred), refresh)
	return t.IDToken, t.Expiry, err
}

// Token returns a valid ID token for cfg: the stored one, else one from the
// refresh token, else, when the client may sign in, one from a new sign-in.
// With refresh set, a stored ID token is not reused.
func (c *Client) Token(ctx context.Context, cfg Config, refresh bool) (Token, error) {
	cfg = cfg.normalised()
	if !refresh {
		if t, ok := c.stored(cfg); ok {
			return t, nil
		}
	}
	unlock, err := c.lock(ctx, cfg)
	if err != nil {
		return Token{}, err
	}
	defer unlock()
	// Another process may have signed in or refreshed while this one
	// waited for the lock.
	if !refresh {
		if t, ok := c.stored(cfg); ok {
			return t, nil
		}
	}
	if rt, err := c.store.Get(credentials.OIDCKey(cfg.id(), "refresh")); err == nil && rt != "" {
		t, err := c.refresh(ctx, cfg, rt)
		if err == nil {
			return t, nil
		}
		if !errors.Is(err, errRefreshRejected) {
			return Token{}, err
		}
		c.log.Debug("refresh token rejected; signing in again", "issuer", cfg.Issuer, "err", err)
		c.forget(cfg)
	}
	if c.ui == nil {
		return Token{}, fmt.Errorf("no sign-in to %s: %w", cfg.Issuer, credentials.ErrLoginRequired)
	}
	return c.login(ctx, cfg, *c.ui)
}

// Login signs in to cfg's issuer now, even when a stored token is still
// valid, and keeps the new tokens.
func (c *Client) Login(ctx context.Context, cfg Config, ui UI) (Token, error) {
	cfg = cfg.normalised()
	unlock, err := c.lock(ctx, cfg)
	if err != nil {
		return Token{}, err
	}
	defer unlock()
	return c.login(ctx, cfg, ui)
}

// Logout forgets the tokens kept for cfg and reports whether there were any.
// The issuer is not told, so the refresh token stays valid there until it
// expires.
func (c *Client) Logout(cfg Config) (bool, error) {
	cfg = cfg.normalised()
	var had bool
	var errs []error
	for _, part := range []string{"refresh", "id"} {
		key := credentials.OIDCKey(cfg.id(), part)
		if _, err := c.store.Get(key); err == nil {
			had = true
		}
		if err := c.store.Delete(key); err != nil && !errors.Is(err, credentials.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return had, errors.Join(errs...)
}

// stored returns the kept ID token while it is valid.
func (c *Client) stored(cfg Config) (Token, bool) {
	raw, err := c.store.Get(credentials.OIDCKey(cfg.id(), "id"))
	if err != nil || raw == "" {
		return Token{}, false
	}
	t, err := parseToken(raw)
	if err != nil || !c.now().Add(credentials.Skew).Before(t.Expiry) {
		return Token{}, false
	}
	return t, true
}

// save keeps the tokens of a sign-in or refresh. A refresh token that cannot
// be kept means signing in again later, so it is logged; the ID token is
// returned either way.
func (c *Client) save(cfg Config, t Token, refreshToken string) {
	if err := c.store.Set(credentials.OIDCKey(cfg.id(), "id"), t.IDToken); err != nil {
		c.log.Debug("keeping the ID token", "issuer", cfg.Issuer, "err", err)
	}
	if refreshToken == "" {
		return
	}
	if err := c.store.Set(credentials.OIDCKey(cfg.id(), "refresh"), refreshToken); err != nil {
		c.log.Warn("keeping the refresh token; the next sign-in will ask again", "issuer", cfg.Issuer, "err", err)
	}
}

func (c *Client) forget(cfg Config) {
	if _, err := c.Logout(cfg); err != nil {
		c.log.Debug("forgetting tokens", "issuer", cfg.Issuer, "err", err)
	}
}

// errRefreshRejected means the issuer no longer accepts the refresh token,
// so only a new sign-in helps.
var errRefreshRejected = errors.New("refresh token rejected")

func (c *Client) refresh(ctx context.Context, cfg Config, refreshToken string) (Token, error) {
	d, err := c.discover(ctx, cfg.Issuer)
	if err != nil {
		return Token{}, err
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)
	tok, err := d.oauth2(cfg, "").TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode < 500 {
			return Token{}, fmt.Errorf("%w: %w", errRefreshRejected, err)
		}
		return Token{}, provider.Errorf(provider.Unavailable, "refreshing the sign-in to %s: %v", cfg.Issuer, err)
	}
	t, err := idToken(tok)
	if err != nil {
		// Some issuers send an ID token only at sign-in.
		return Token{}, fmt.Errorf("%w: %w", errRefreshRejected, err)
	}
	c.save(cfg, t, tok.RefreshToken)
	return t, nil
}

// lock serialises sign-ins and refreshes for one issuer, client and scopes
// across tug processes, such as several kubectl calls at once.
func (c *Client) lock(ctx context.Context, cfg Config) (func(), error) {
	if c.lockDir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(c.lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the sign-in lock directory: %w", err)
	}
	path := filepath.Join(c.lockDir, cfg.id()+".lock")
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		unlock, err := filelock.TryLock(path)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, filelock.ErrLocked) {
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}
