// Package guard enforces read-only sessions and records in the audit log the
// writes it lets through, how each was answered, and the ones it refuses. It
// sits in the HTTP transport of every client tug builds, so tug commands
// cannot skip it.
package guard

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/paingha/tugcli/internal/audit"
)

// Recorder notes a write the guard lets through, before it is sent, its
// outcome, and a write it refuses.
type Recorder interface {
	Record(ctx context.Context, e audit.Entry) error
}

// Policy decides whether writes are allowed on a cluster.
type Policy struct {
	Cluster string
	// ClusterID and Session name the catalog cluster and the tug session
	// in audit entries.
	ClusterID     string
	Session       string
	ReadOnly      bool
	UnlockedUntil time.Time
	// Now defaults to time.Now.
	Now func() time.Time
	// Confirm, when set, is asked once before the first write on a read-only
	// session. Returning true allows writes for the rest of the command.
	Confirm func(action string) bool
	// Audit, when set, records every write the guard lets through and
	// refuses. A write that cannot be recorded is not sent.
	Audit Recorder
	// Intent is the command the writes come from, such as "tug restart",
	// for audit entries.
	Intent string
	// Log, when set, hears about outcomes and refusals the audit log could
	// not take.
	Log *slog.Logger
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
	// Unlock is how long the error suggests unlocking for, such as a
	// policy's limit; zero suggests DefaultUnlock.
	Unlock time.Duration
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("refused %s: %s is read-only; run `tug unlock %s` to allow writes",
		e.Action, e.Cluster, ShortDuration(cmp.Or(e.Unlock, DefaultUnlock)))
}

// Writable reports whether writes are allowed now without asking.
func (p *Policy) Writable() bool { return p.allowance() != "" }

// allowance says how writes are allowed now without asking, as an audit
// entry puts it, or "" when they are not.
func (p *Policy) allowance() string {
	switch {
	case !p.ReadOnly:
		return audit.AllowedReadWrite
	case p.now().Before(p.UnlockedUntil):
		return audit.AllowedUnlocked
	}
	return ""
}

func (p *Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Wrap returns a RoundTripper that refuses writes when p does not allow them.
func Wrap(rt http.RoundTripper, p *Policy) http.RoundTripper {
	return WrapBase(rt, p, "")
}

// WrapBase is Wrap for an API server whose URL has a path, such as Rancher's
// /k8s/clusters/<id> or tsh's local proxy: requests are judged by their
// path below base, which is where the Kubernetes API's paths start.
func WrapBase(rt http.RoundTripper, p *Policy, base string) http.RoundTripper {
	return &roundTripper{next: rt, policy: p, base: strings.TrimSuffix(base, "/")}
}

type roundTripper struct {
	next   http.RoundTripper
	policy *Policy
	base   string

	mu        sync.Mutex
	confirmed bool
}

func (g *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	u := g.apiURL(req.URL)
	if IsRead(req.Method, u) {
		return g.next.RoundTrip(req)
	}
	allowed := g.policy.allowance()
	if allowed == "" {
		action := Describe(req.Method, u)
		if !g.allowedByConfirm(action) {
			closeBody(req)
			g.note(req.Context(), g.entry(audit.KindRefused, req, u, func(e *audit.Entry) { e.Reason = audit.ReasonReadOnly }))
			return nil, &DeniedError{Cluster: g.policy.Cluster, Action: action}
		}
		allowed = audit.AllowedConfirmed
	}
	id, err := g.record(req, u, allowed)
	if err != nil {
		closeBody(req)
		return nil, err
	}
	resp, err := g.next.RoundTrip(req)
	if id != "" {
		g.note(req.Context(), g.entry(audit.KindResult, req, u, func(e *audit.Entry) {
			e.ID = id
			if err != nil {
				e.Reason = audit.ReasonNoResponse
			} else {
				e.Status = resp.StatusCode
			}
		}))
	}
	return resp, err
}

// entry is an audit entry of a kind about request req to API path u, which
// set finishes.
func (g *roundTripper) entry(kind string, req *http.Request, u *url.URL, set func(*audit.Entry)) audit.Entry {
	p := g.policy
	e := audit.Entry{
		Time:      p.now(),
		Kind:      kind,
		Source:    audit.SourceTug,
		Cluster:   p.Cluster,
		ClusterID: p.ClusterID,
		Session:   p.Session,
		Namespace: audit.NamespaceOf(u.Path),
		Intent:    p.Intent,
		Method:    req.Method,
		Path:      u.Path,
	}
	set(&e)
	return e
}

// record notes a write in the policy's audit log before it is sent: the
// method and the API path u, below any base path, never the query string
// or the body. It returns the write's ID, or "" without an audit log.
func (g *roundTripper) record(req *http.Request, u *url.URL, allowed string) (string, error) {
	if g.policy.Audit == nil {
		return "", nil
	}
	id := audit.NewID()
	err := g.policy.Audit.Record(req.Context(), g.entry(audit.KindWrite, req, u, func(e *audit.Entry) {
		e.ID, e.Allowed = id, allowed
	}))
	if err != nil {
		return "", fmt.Errorf("not sending %s, since it could not be recorded in the audit log: %w", Describe(req.Method, u), err)
	}
	return id, nil
}

// noteTimeout bounds how long recording an outcome or a refusal waits for
// the audit log.
const noteTimeout = 5 * time.Second

// note records an outcome or a refusal. The request has been sent or
// refused either way, so a failure is only logged; a cancelled request
// still has its outcome recorded.
func (g *roundTripper) note(ctx context.Context, e audit.Entry) {
	if g.policy.Audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noteTimeout)
	defer cancel()
	if err := g.policy.Audit.Record(ctx, e); err != nil && g.policy.Log != nil {
		g.policy.Log.Warn("recording in the audit log", "kind", e.Kind, "err", err)
	}
}

// closeBody closes the body of a request that is not sent, as a
// RoundTripper must.
func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// apiURL returns u with the server's base path taken off.
func (g *roundTripper) apiURL(u *url.URL) *url.URL {
	if g.base == "" {
		return u
	}
	rest, ok := strings.CutPrefix(u.Path, g.base)
	if !ok || rest != "" && rest[0] != '/' {
		return u
	}
	api := *u
	api.Path, api.RawPath = rest, ""
	return &api
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

// Kubelet endpoints, reached through the API server's nodes/<name>/proxy,
// that run code in or attach to a container. The kubelet accepts exec and
// attach as WebSocket GETs, so the method alone does not tell.
var kubeletWriteEndpoints = []string{"exec", "run", "attach", "checkpoint"}

// Review APIs that only answer questions about the caller.
var readOnlyPosts = []string{
	"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews",
	"/apis/authorization.k8s.io/v1/selfsubjectrulesreviews",
	"/apis/authentication.k8s.io/v1/selfsubjectreviews",
	"/apis/authentication.k8s.io/v1beta1/selfsubjectreviews",
}

// IsRead reports whether a request only reads. Port-forwards count as reads:
// they are how people debug read-only clusters. Server-side dry runs count as
// reads because nothing is persisted. A path with empty, "." or ".." segments
// is never a read: Kubernetes clients do not send one, and the guard should
// not have to guess how the API server or a kubelet would resolve it.
func IsRead(method string, u *url.URL) bool {
	p := strings.TrimSuffix(u.Path, "/")
	if p != "" && path.Clean(p) != p {
		return false
	}
	last := p[strings.LastIndex(p, "/")+1:]
	if slices.Contains(writeSubresources, last) || runsOnKubelet(p) {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		if last == "portforward" || slices.Contains(readOnlyPosts, p) {
			return true
		}
	}
	return u.Query().Get("dryRun") == "All"
}

// runsOnKubelet reports whether path goes through nodes/<name>/proxy to a
// kubelet endpoint that runs code or attaches to a container, such as
// /api/v1/nodes/n1/proxy/exec/<ns>/<pod>/<container>. Any segment after
// proxy counts, in any case, so that an unusual path errs towards a write.
func runsOnKubelet(p string) bool {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] != "nodes" || parts[i+2] != "proxy" {
			continue
		}
		for _, seg := range parts[i+3:] {
			if slices.Contains(kubeletWriteEndpoints, strings.ToLower(seg)) {
				return true
			}
		}
	}
	return false
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
