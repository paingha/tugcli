// Package guard enforces read-only sessions. It sits in the HTTP transport of
// every client tug builds, so tug commands cannot skip it.
package guard

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Policy decides whether writes are allowed on a cluster.
type Policy struct {
	Cluster       string
	ReadOnly      bool
	UnlockedUntil time.Time
	// Now defaults to time.Now.
	Now func() time.Time
	// Confirm, when set, is asked once before the first write on a read-only
	// session. Returning true allows writes for the rest of the command.
	Confirm func(action string) bool
}

// ReadOnlyFor reports whether a cluster with these labels should open
// read-only: it does when it matches any of the selectors.
func ReadOnlyFor(labels map[string]string, selectors []map[string]string) bool {
	for _, sel := range selectors {
		if len(sel) == 0 {
			continue
		}
		match := true
		for k, v := range sel {
			if labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// DeniedError is returned for a write on a read-only session.
type DeniedError struct {
	Cluster string
	Action  string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("refused %s: %s is read-only; run `tug unlock 15m` to allow writes", e.Action, e.Cluster)
}

// Writable reports whether writes are allowed now without asking.
func (p *Policy) Writable() bool {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	return !p.ReadOnly || now().Before(p.UnlockedUntil)
}

// Wrap returns a RoundTripper that refuses writes when p does not allow them.
func Wrap(rt http.RoundTripper, p *Policy) http.RoundTripper {
	return &roundTripper{next: rt, policy: p}
}

type roundTripper struct {
	next   http.RoundTripper
	policy *Policy

	mu        sync.Mutex
	confirmed bool
}

func (g *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if IsRead(req.Method, req.URL) || g.policy.Writable() {
		return g.next.RoundTrip(req)
	}
	action := Describe(req.Method, req.URL)
	if g.allowedByConfirm(action) {
		return g.next.RoundTrip(req)
	}
	return nil, &DeniedError{Cluster: g.policy.Cluster, Action: action}
}

func (g *roundTripper) allowedByConfirm(action string) bool {
	if g.policy.Confirm == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.confirmed {
		g.confirmed = g.policy.Confirm(action)
	}
	return g.confirmed
}

// Subresources that run code or attach to containers count as writes even
// when requested with GET, as WebSocket upgrades are.
var writeSubresources = []string{"exec", "attach"}

// Review APIs that only answer questions about the caller.
var readOnlyPosts = []string{
	"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews",
	"/apis/authorization.k8s.io/v1/selfsubjectrulesreviews",
	"/apis/authentication.k8s.io/v1/selfsubjectreviews",
	"/apis/authentication.k8s.io/v1beta1/selfsubjectreviews",
}

// IsRead reports whether a request only reads. Port-forwards count as reads:
// they are how people debug read-only clusters. Server-side dry runs count as
// reads because nothing is persisted.
func IsRead(method string, u *url.URL) bool {
	path := strings.TrimSuffix(u.Path, "/")
	last := path[strings.LastIndex(path, "/")+1:]
	for _, s := range writeSubresources {
		if last == s {
			return false
		}
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		if last == "portforward" {
			return true
		}
		for _, p := range readOnlyPosts {
			if path == p {
				return true
			}
		}
	}
	return u.Query().Get("dryRun") == "All"
}

// Describe names a request for messages, for example "DELETE pods/api-0".
func Describe(method string, u *url.URL) string {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// /api/v1/namespaces/ns/pods/name[/sub] or /apis/g/v/namespaces/ns/r/name
	i := 0
	if len(parts) > 0 && parts[0] == "api" {
		i = 2
	} else if len(parts) > 0 && parts[0] == "apis" {
		i = 3
	}
	if i+1 < len(parts) && parts[i] == "namespaces" && i+2 < len(parts) {
		i += 2
	}
	if i < len(parts) {
		return method + " " + strings.Join(parts[i:], "/")
	}
	return method + " " + u.Path
}
