package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/paingha/tugcli/pkg/provider"
)

// discovery is the part of an issuer's OpenID configuration tug uses.
type discovery struct {
	Issuer                      string   `json:"issuer"`
	AuthorizationEndpoint       string   `json:"authorization_endpoint"`
	TokenEndpoint               string   `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint"`
	CodeChallengeMethods        []string `json:"code_challenge_methods_supported"`
}

// discover fetches the issuer's OpenID configuration.
func (c *Client) discover(ctx context.Context, issuer string) (*discovery, error) {
	if err := checkIssuer(issuer); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, provider.Errorf(provider.Unavailable, "reaching %s: %v", issuer, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, provider.Errorf(provider.Unavailable, "reading the OpenID configuration of %s: %v", issuer, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, provider.Errorf(provider.Unavailable, "%s answered %s for its OpenID configuration", issuer, resp.Status)
	}
	var d discovery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "the OpenID configuration of %s is not JSON: %v", issuer, err)
	}
	// The issuer in the document must be the one asked for, or tokens
	// could come from somewhere the cluster does not trust.
	if strings.TrimRight(d.Issuer, "/") != issuer {
		return nil, provider.Errorf(provider.InvalidConfig, "the OpenID configuration of %s names the issuer %q", issuer, d.Issuer)
	}
	if d.TokenEndpoint == "" {
		return nil, provider.Errorf(provider.InvalidConfig, "the OpenID configuration of %s has no token endpoint", issuer)
	}
	return &d, nil
}

// checkIssuer allows https issuers, and http only on this machine, as a
// local test issuer would be.
func checkIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return provider.Errorf(provider.InvalidConfig, "the issuer %q is not a URL", issuer)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return nil
	default:
		return provider.Errorf(provider.InvalidConfig, "the issuer %s must use https", issuer)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// pkce reports whether the issuer says it supports S256 code challenges.
// Like kubelogin, tug uses PKCE only when the issuer advertises it.
func (d *discovery) pkce() bool { return slices.Contains(d.CodeChallengeMethods, "S256") }

func (d *discovery) oauth2(cfg Config, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Scopes:       cfg.Scopes,
		RedirectURL:  redirectURL,
		Endpoint: oauth2.Endpoint{
			AuthURL:       d.AuthorizationEndpoint,
			TokenURL:      d.TokenEndpoint,
			DeviceAuthURL: d.DeviceAuthorizationEndpoint,
		},
	}
}

// claims are the ID token claims tug reads. tug does not check the token's
// signature: the API server does that. It reads the expiry to know when to
// ask again, the nonce to tie a sign-in to its token, and who signed in, to
// say so.
type claims struct {
	Expiry  float64 `json:"exp"`
	Nonce   string  `json:"nonce"`
	Subject string  `json:"sub"`
	Email   string  `json:"email"`
}

func parseClaims(raw string) (claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return claims{}, errors.New("the ID token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return claims{}, fmt.Errorf("decoding the ID token: %w", err)
	}
	var cl claims
	if err := json.Unmarshal(payload, &cl); err != nil {
		return claims{}, fmt.Errorf("decoding the ID token's claims: %w", err)
	}
	if cl.Expiry <= 0 {
		return claims{}, errors.New("the ID token has no expiry")
	}
	return cl, nil
}

func parseToken(raw string) (Token, error) {
	cl, err := parseClaims(raw)
	if err != nil {
		return Token{}, err
	}
	who := cl.Email
	if who == "" {
		who = cl.Subject
	}
	return Token{IDToken: raw, Expiry: time.Unix(int64(cl.Expiry), 0).UTC(), Identity: who}, nil
}

// idToken takes the ID token out of a token response.
func idToken(tok *oauth2.Token) (Token, error) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Token{}, errors.New("the issuer sent no ID token; check that the client may use the openid scope")
	}
	return parseToken(raw)
}
