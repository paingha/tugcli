// Package oidctest is a fake OpenID Connect issuer for tests. It signs
// nothing: tug reads ID tokens without checking signatures, as the API
// server checks them.
package oidctest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ClientID is the only client the issuer knows.
const ClientID = "tug-test"

// Email is who signs in.
const Email = "dev@example.com"

// Issuer is a fake issuer serving discovery, authorization, device and
// token endpoints over http on 127.0.0.1.
type Issuer struct {
	URL string

	// mu guards everything below. Change settings with Set once the issuer
	// may be serving.
	mu sync.Mutex
	// Lifetime is how long ID tokens are valid; the default is an hour.
	Lifetime time.Duration
	// Now is the issuer's clock.
	Now func() time.Time
	// RejectRefresh answers every refresh with invalid_grant.
	RejectRefresh bool
	// WrongState and WrongNonce make the issuer misbehave in a sign-in.
	WrongState, WrongNonce bool
	// NoPKCE leaves S256 out of the discovery document.
	NoPKCE bool
	// NoDevice leaves the device authorization endpoint out of it.
	NoDevice bool

	logins, refreshes int
	challenges        []string

	codes   map[string]grant
	refresh map[string]bool
	seq     int
}

type grant struct {
	nonce, challenge, redirect string
}

// NewIssuer starts an issuer that stops when the test ends.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	iss := &Issuer{Lifetime: time.Hour, Now: time.Now, codes: map[string]grant{}, refresh: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(iss.serve))
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	return iss
}

// Counts returns how many tokens were issued by sign-in and by refresh.
func (iss *Issuer) Counts() (logins, refreshes int) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	return iss.logins, iss.refreshes
}

// Challenges returns the PKCE challenge of each authorization request, ""
// for a request without one.
func (iss *Issuer) Challenges() []string {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	return append([]string(nil), iss.challenges...)
}

// Set changes the issuer's settings while it may be serving.
func (iss *Issuer) Set(change func(*Issuer)) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	change(iss)
}

// Browser returns a stand-in for a browser: it follows the sign-in address
// through the issuer to tug's loopback redirect.
func (iss *Issuer) Browser() func(context.Context, string) error {
	return func(ctx context.Context, u string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
}

func (iss *Issuer) serve(w http.ResponseWriter, r *http.Request) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		methods := []string{"S256"}
		if iss.NoPKCE {
			methods = nil
		}
		doc := map[string]any{
			"issuer":                           iss.URL,
			"authorization_endpoint":           iss.URL + "/authorize",
			"token_endpoint":                   iss.URL + "/token",
			"device_authorization_endpoint":    iss.URL + "/device",
			"code_challenge_methods_supported": methods,
		}
		if iss.NoDevice {
			delete(doc, "device_authorization_endpoint")
		}
		writeJSON(w, http.StatusOK, doc)
	case "/authorize":
		iss.authorize(w, r)
	case "/device":
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code": "device-code", "user_code": "ABCD-EFGH",
			"verification_uri": iss.URL + "/activate", "interval": 1, "expires_in": 300,
		})
	case "/token":
		iss.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (iss *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != ClientID || !strings.HasPrefix(redirect, "http://localhost:") || q.Get("response_type") != "code" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	iss.seq++
	code := fmt.Sprintf("code-%d", iss.seq)
	iss.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: redirect}
	iss.challenges = append(iss.challenges, q.Get("code_challenge"))
	state := q.Get("state")
	if iss.WrongState {
		state = "not-" + state
	}
	http.Redirect(w, r, redirect+"/?"+url.Values{"code": {code}, "state": {state}}.Encode(), http.StatusFound)
}

func (iss *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if id, _, ok := r.BasicAuth(); (ok && id != ClientID) || (!ok && r.PostForm.Get("client_id") != ClientID) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		g, ok := iss.codes[r.PostForm.Get("code")]
		delete(iss.codes, r.PostForm.Get("code"))
		if !ok || r.PostForm.Get("redirect_uri") != g.redirect || !verifies(r.PostForm.Get("code_verifier"), g.challenge) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		iss.logins++
		nonce := g.nonce
		if iss.WrongNonce {
			nonce = "not-" + nonce
		}
		iss.issue(w, nonce)
	case "refresh_token":
		rt := r.PostForm.Get("refresh_token")
		if iss.RejectRefresh || !iss.refresh[rt] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		delete(iss.refresh, rt) // refresh tokens are rotated
		iss.refreshes++
		iss.issue(w, "")
	case "urn:ietf:params:oauth:grant-type:device_code":
		if r.PostForm.Get("device_code") != "device-code" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		iss.logins++
		iss.issue(w, "")
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

// verifies checks a PKCE verifier against its S256 challenge. A request
// without a challenge needs no verifier.
func verifies(verifier, challenge string) bool {
	if challenge == "" {
		return verifier == ""
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

func (iss *Issuer) issue(w http.ResponseWriter, nonce string) {
	iss.seq++
	rt := fmt.Sprintf("refresh-%d", iss.seq)
	iss.refresh[rt] = true
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  fmt.Sprintf("access-%d", iss.seq),
		"token_type":    "Bearer",
		"expires_in":    int(iss.Lifetime.Seconds()),
		"refresh_token": rt,
		"id_token":      iss.IDToken(nonce),
	})
}

// IDToken returns an unsigned ID token from this issuer.
func (iss *Issuer) IDToken(nonce string) string {
	now := iss.Now()
	claims := map[string]any{
		"iss": iss.URL, "aud": ClientID, "sub": "user-1", "email": Email,
		"iat": now.Unix(), "exp": now.Add(iss.Lifetime).Unix(), "seq": iss.seq,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(header) + "." + enc(payload) + ".unsigned"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
