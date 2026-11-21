// Package apiclient is the small JSON-over-HTTPS client the first-party
// provider plugins share. It depends only on the standard library and the
// public SDK, like the plugins themselves.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// ConfigAPIURL is the account setting that points a plugin at another API
// endpoint, such as a regional or test endpoint.
const ConfigAPIURL = "apiURL"

// Client calls one provider API with a bearer token.
type Client struct {
	BaseURL string
	// Token is sent as a bearer token, unless Sign is set.
	Token string
	// Sign, when set, authenticates each request in place of Token, for
	// APIs that sign requests. It is called once the request is complete.
	Sign      func(req *http.Request) error
	HTTP      *http.Client
	UserAgent string
	// Message extracts a readable error message from an error body.
	Message func(body []byte) string
	// Code, when set, picks the code of an error response whose status
	// alone does not say what went wrong, such as a 403 for a key that does
	// not exist. It returns "" to keep the code the status maps to.
	Code func(status int, body []byte) provider.Code
}

// BaseURL returns the account's apiURL setting, or def. It must use https,
// except on a loopback address for local testing.
func BaseURL(acct provider.Account, def string) (string, error) {
	raw := acct.ConfigString(ConfigAPIURL)
	if raw == "" {
		return def, nil
	}
	return CheckURL(ConfigAPIURL, raw)
}

// CheckURL checks raw, the value of the account setting key, the way
// BaseURL checks apiURL, and returns it without a trailing slash.
func CheckURL(key, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", provider.Errorf(provider.InvalidConfig, "%s %q is not a URL", key, raw)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return "", provider.Errorf(provider.InvalidConfig, "%s must use https", key)
	}
	return strings.TrimSuffix(raw, "/"), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RawBody is a request body that Send passes on unchanged with its content
// type, for APIs that take something other than JSON, such as a form.
type RawBody struct {
	ContentType string
	Data        []byte
}

// Response is a successful response: its headers, for APIs that page
// through them, and its body.
type Response struct {
	Header http.Header
	Body   []byte
}

// Do sends a request and decodes a JSON response into out (which may be
// nil). Non-2xx responses become *provider.Error with the matching code.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.Send(ctx, method, path, body)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(resp.Body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return provider.Errorf(provider.Internal, "decoding %s: %v", path, err)
	}
	return nil
}

// Send sends a request and returns the response. body is encoded as JSON
// unless it is a RawBody, and nil sends none. Non-2xx responses become
// *provider.Error with the code their status maps to, or the one Code picks.
func (c *Client) Send(ctx context.Context, method, path string, body any) (*Response, error) {
	var (
		reqBody     io.Reader
		contentType string
	)
	switch b := body.(type) {
	case nil:
	case RawBody:
		reqBody, contentType = bytes.NewReader(b.Data), b.ContentType
	default:
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody, contentType = bytes.NewReader(data), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if c.Sign != nil {
		if err := c.Sign(req); err != nil {
			return nil, err
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, provider.Errorf(provider.Unavailable, "%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, provider.MaxMessageBytes))
	if err != nil {
		return nil, provider.Errorf(provider.Unavailable, "reading %s: %v", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		if c.Message != nil {
			if m := c.Message(data); m != "" {
				msg = m
			}
		}
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		e := provider.StatusError(resp.StatusCode, fmt.Sprintf("%s %s: %s", method, path, msg), resp.Header)
		if c.Code != nil {
			if code := c.Code(resp.StatusCode, data); code != "" {
				e.Code = code
			}
		}
		return nil, e
	}
	return &Response{Header: resp.Header, Body: data}, nil
}
