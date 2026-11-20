package oidc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/paingha/tugcli/pkg/provider"
)

// login signs in with the browser, or with a device code when ui asks for
// it, and keeps the tokens. The caller holds the lock.
func (c *Client) login(ctx context.Context, cfg Config, ui UI) (Token, error) {
	d, err := c.discover(ctx, cfg.Issuer)
	if err != nil {
		return Token{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)

	if ui.Device && d.DeviceAuthorizationEndpoint == "" {
		ui.Tell(fmt.Sprintf("%s does not offer device sign-in, so tug signs in with a browser on this machine. Over SSH, forward the port first: ssh -L 8000:localhost:8000", cfg.Issuer))
		ui.Device = false
	}
	var t Token
	var refreshToken string
	if ui.Device {
		t, refreshToken, err = c.deviceLogin(ctx, cfg, d, ui)
	} else {
		t, refreshToken, err = c.browserLogin(ctx, cfg, d, ui)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Token{}, provider.Errorf(provider.Unauthenticated, "signing in to %s took longer than %s", cfg.Issuer, c.timeout)
	}
	if err != nil {
		return Token{}, err
	}
	c.save(cfg, t, refreshToken)
	return t, nil
}

// callback is what the browser brought back to the loopback redirect.
type callback struct {
	code string
	err  error
}

func (c *Client) browserLogin(ctx context.Context, cfg Config, d *discovery, ui UI) (Token, string, error) {
	if d.AuthorizationEndpoint == "" {
		return Token{}, "", provider.Errorf(provider.InvalidConfig, "the OpenID configuration of %s has no authorization endpoint", cfg.Issuer)
	}
	ln, err := c.listenLoopback()
	if err != nil {
		return Token{}, "", err
	}
	redirect := fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)
	oc := d.oauth2(cfg, redirect)

	state, nonce := randomString(), randomString()
	verifier := oauth2.GenerateVerifier()
	authOpts := []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("nonce", nonce)}
	var exchangeOpts []oauth2.AuthCodeOption
	if d.pkce() {
		authOpts = append(authOpts, oauth2.S256ChallengeOption(verifier))
		exchangeOpts = append(exchangeOpts, oauth2.VerifierOption(verifier))
	}
	authURL := oc.AuthCodeURL(state, authOpts...)

	result := make(chan callback, 1)
	srv := &http.Server{Handler: callbackHandler(state, result), ReadHeaderTimeout: 10 * time.Second}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = srv.Serve(ln) // returns http.ErrServerClosed after Close
	}()
	defer func() {
		_ = srv.Close()
		<-serveDone
	}()

	ui.Tell(fmt.Sprintf("Sign in to %s in your browser. If it does not open, visit:\n  %s", cfg.Issuer, authURL))
	if ui.OpenBrowser != nil {
		if err := ui.OpenBrowser(ctx, authURL); err != nil {
			ui.Tell(fmt.Sprintf("Could not open a browser (%v); open the address above.", err))
		}
	}

	var cb callback
	select {
	case <-ctx.Done():
		return Token{}, "", ctx.Err()
	case cb = <-result:
	}
	if cb.err != nil {
		return Token{}, "", cb.err
	}
	tok, err := oc.Exchange(ctx, cb.code, exchangeOpts...)
	if err != nil {
		return Token{}, "", provider.Errorf(provider.Unauthenticated, "exchanging the sign-in code with %s: %v", cfg.Issuer, err)
	}
	t, err := idToken(tok)
	if err != nil {
		return Token{}, "", err
	}
	// The nonce ties the ID token to this sign-in, so a token minted for
	// another request is not accepted.
	if cl, err := parseClaims(t.IDToken); err != nil || cl.Nonce != nonce {
		return Token{}, "", provider.Errorf(provider.Unauthenticated, "the ID token from %s does not belong to this sign-in (nonce mismatch)", cfg.Issuer)
	}
	return t, tok.RefreshToken, nil
}

// listenLoopback listens on the first free address for the redirect.
func (c *Client) listenLoopback() (net.Listener, error) {
	var errs []error
	for _, addr := range c.listen {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("listening for the sign-in redirect on %v: %w; free one of those ports or set oidc.listenAddresses in config.yaml",
		c.listen, errors.Join(errs...))
}

// callbackHandler answers the browser's redirect and passes the first
// result on. Later requests, such as a reload, are told the sign-in is over.
func callbackHandler(state string, result chan<- callback) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var cb callback
		switch {
		case q.Get("state") != state:
			cb.err = provider.Errorf(provider.Unauthenticated, "the sign-in redirect carried the wrong state; start again")
		case q.Get("error") != "":
			msg := q.Get("error")
			if desc := q.Get("error_description"); desc != "" {
				msg += ": " + desc
			}
			cb.err = provider.Errorf(provider.Unauthenticated, "the issuer refused the sign-in: %s", msg)
		case q.Get("code") == "":
			cb.err = provider.Errorf(provider.Unauthenticated, "the sign-in redirect carried no code")
		default:
			cb.code = q.Get("code")
		}
		sent := false
		once.Do(func() {
			result <- cb
			sent = true
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case !sent:
			fmt.Fprint(w, page("This sign-in is already over. You can close this tab."))
		case cb.err != nil:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, page("tug could not sign you in: "+cb.err.Error()))
		default:
			fmt.Fprint(w, page("Signed in. You can close this tab and go back to the terminal."))
		}
	})
}

func page(msg string) string {
	return "<!doctype html><meta charset=utf-8><title>tug</title><p>" + html.EscapeString(msg) + "</p>\n"
}

func (c *Client) deviceLogin(ctx context.Context, cfg Config, d *discovery, ui UI) (Token, string, error) {
	oc := d.oauth2(cfg, "")
	da, err := oc.DeviceAuth(ctx)
	if err != nil {
		return Token{}, "", provider.Errorf(provider.Unauthenticated, "starting device sign-in with %s: %v", cfg.Issuer, err)
	}
	where := da.VerificationURI
	if da.VerificationURIComplete != "" {
		where = da.VerificationURIComplete
	}
	ui.Tell(fmt.Sprintf("To sign in to %s, open %s on any device and enter the code %s", cfg.Issuer, where, da.UserCode))
	tok, err := oc.DeviceAccessToken(ctx, da)
	if err != nil {
		if ctx.Err() != nil {
			return Token{}, "", ctx.Err()
		}
		return Token{}, "", provider.Errorf(provider.Unauthenticated, "device sign-in with %s: %v", cfg.Issuer, err)
	}
	t, err := idToken(tok)
	if err != nil {
		return Token{}, "", err
	}
	return t, tok.RefreshToken, nil
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return base64.RawURLEncoding.EncodeToString(b)
}
