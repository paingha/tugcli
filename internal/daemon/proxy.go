package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/audit"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/pkg/provider"
)

// upstream is a reverse proxy to one cluster's API server.
type upstream struct {
	proxy *httputil.ReverseProxy
	// rt is the proxy's transport, whose idle connections are closed when
	// the upstream is replaced or the machine wakes.
	rt      http.RoundTripper
	created time.Time
	// refreshed says its credential was asked anew of the provider after
	// the cluster refused the one before.
	refreshed bool
}

// proxyRequest is what tugd knows about a request it forwards, for the
// handlers of its response.
type proxyRequest struct {
	sess *session.Session
}

type proxyRequestKey struct{}

// ProxyHandler serves /s/<session>/... by forwarding to the session's
// cluster with its real credential, after the session token and the
// read-only guard have been checked.
func (s *Server) ProxyHandler() http.Handler { return http.HandlerFunc(s.serveProxy) }

func (s *Server) serveProxy(w http.ResponseWriter, r *http.Request) {
	sessionID, rest, ok := splitSession(r.URL.Path)
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "tug proxy: paths start with /s/<session>")
		return
	}
	if !s.validToken(sessionID, bearer(r)) {
		writeStatus(w, http.StatusUnauthorized, metav1.StatusReasonUnauthorized,
			"tug proxy: missing or wrong session token; use the kubeconfig of a tug session")
		return
	}
	sess, ok := s.usesProxy(sessionID)
	if !ok {
		writeStatus(w, http.StatusGone, metav1.StatusReasonGone,
			"tug proxy: this tug session has ended, is not connected, or is not using the proxy; run `tug connect --proxy`")
		return
	}

	target := *r.URL
	target.Path, target.RawPath = rest, ""
	if !guard.IsRead(r.Method, &target) && !sess.State.Writable(s.cfg.Now()) {
		denied := &guard.DeniedError{Cluster: sess.State.ClusterName, Action: guard.Describe(r.Method, &target)}
		writeStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden, "tug: "+denied.Error())
		return
	}

	up, err := s.upstream(r.Context(), sess.State.ClusterID)
	if err != nil {
		code, reason := http.StatusBadGateway, metav1.StatusReasonServiceUnavailable
		if provider.CodeOf(err) == provider.Unauthenticated {
			code, reason = http.StatusUnauthorized, metav1.StatusReasonUnauthorized
		}
		writeStatus(w, code, reason, fmt.Sprintf("tug proxy: cannot forward to %s: %s", sess.State.ClusterName, withHint(err)))
		return
	}
	if !guard.IsRead(r.Method, &target) {
		if err := s.record(r, sess, target.Path); err != nil {
			writeStatus(w, http.StatusInternalServerError, metav1.StatusReasonInternalError,
				fmt.Sprintf("tug proxy: not forwarding %s, since it could not be recorded in the audit log: %v",
					guard.Describe(r.Method, &target), err))
			return
		}
	}
	out := r.Clone(context.WithValue(r.Context(), proxyRequestKey{}, &proxyRequest{sess: sess}))
	out.URL.Path, out.URL.RawPath = rest, ""
	// The session token is for tugd only; the upstream gets the cluster's
	// own credential from the transport.
	out.Header.Del("Authorization")
	up.proxy.ServeHTTP(w, out)
}

// withHint is err's message, followed by the hint of a provider error.
func withHint(err error) string {
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Hint != "" {
		return err.Error() + "; " + pe.Hint
	}
	return err.Error()
}

// maxClient bounds the User-Agent kept in an audit entry.
const maxClient = 200

// record notes a write the proxy is about to forward in the audit log: the
// method, the API path and the client's User-Agent, never the query string,
// the body or a credential.
func (s *Server) record(r *http.Request, sess *session.Session, path string) error {
	if s.cfg.Audit == nil {
		return nil
	}
	allowed := audit.AllowedReadWrite
	if sess.State.ReadOnly {
		allowed = audit.AllowedUnlocked // the guard let it through
	}
	client := r.UserAgent()
	if len(client) > maxClient {
		client = client[:maxClient]
	}
	return s.cfg.Audit.Record(r.Context(), audit.Entry{
		Time:      s.cfg.Now(),
		Source:    audit.SourceProxy,
		Cluster:   sess.State.ClusterName,
		ClusterID: sess.State.ClusterID,
		Session:   sess.State.ID,
		Allowed:   allowed,
		Method:    r.Method,
		Path:      path,
		Client:    client,
	})
}

// splitSession splits "/s/<id>/rest" into its session ID and the API path.
func splitSession(p string) (id, rest string, ok bool) {
	tail, found := strings.CutPrefix(p, "/s/")
	if !found {
		return "", "", false
	}
	id, rest, _ = strings.Cut(tail, "/")
	if id == "" {
		return "", "", false
	}
	return id, "/" + rest, true
}

func bearer(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(tok)
}

// upstream returns the cached proxy for a cluster, building a new one when
// there is none or it is older than upstreamTTL. Requests that miss the
// cache together wait for one build, so a client opening many connections
// at once asks for the cluster's credential once.
func (s *Server) upstream(ctx context.Context, clusterID string) (*upstream, error) {
	if up := s.cachedUpstream(clusterID); up != nil {
		return up, nil
	}
	s.mu.Lock()
	build, ok := s.building[clusterID]
	if !ok {
		build = &sync.Mutex{}
		s.building[clusterID] = build
	}
	s.mu.Unlock()
	build.Lock()
	defer build.Unlock()
	if up := s.cachedUpstream(clusterID); up != nil {
		return up, nil // built while this request waited
	}

	s.mu.Lock()
	refresh := s.refresh[clusterID]
	s.mu.Unlock()
	cfg, err := s.cfg.Upstream(ctx, clusterID, refresh)
	if err != nil {
		return nil, err
	}
	up, err := s.newUpstream(clusterID, cfg)
	if err != nil {
		return nil, err
	}
	up.refreshed = refresh
	s.mu.Lock()
	old := s.upstreams[clusterID]
	s.upstreams[clusterID] = up
	delete(s.refresh, clusterID)
	s.mu.Unlock()
	if old != nil {
		// Its streams carry on; new requests use the new upstream.
		utilnet.CloseIdleConnectionsFor(old.rt)
	}
	return up, nil
}

// cachedUpstream returns a cluster's proxy if it is younger than upstreamTTL.
func (s *Server) cachedUpstream(clusterID string) *upstream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if up, ok := s.upstreams[clusterID]; ok && s.cfg.Now().Sub(up.created) < upstreamTTL {
		return up
	}
	return nil
}

// refreshBackoff is how long after asking a provider for a new credential
// tugd waits before asking again, when the cluster refuses the new one too.
const refreshBackoff = 30 * time.Second

// credentialRefused forgets the upstream a cluster refused the credential
// of, so that the next request builds one with a new credential from the
// cluster's provider. It reports whether one will be asked for: not when
// the refused credential is one the provider gave moments ago.
func (s *Server) credentialRefused(clusterID string, up *upstream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.upstreams[clusterID] != up:
		return true // already replaced, perhaps with a new credential
	case up.refreshed && s.cfg.Now().Sub(up.created) < refreshBackoff:
		return false
	}
	delete(s.upstreams, clusterID)
	s.refresh[clusterID] = true
	return true
}

// dropUpstreams forgets every cluster's upstream and closes their idle
// connections, which the network they were opened on may no longer carry:
// after the machine slept, a pooled connection can look open and never
// answer.
func (s *Server) dropUpstreams() {
	s.mu.Lock()
	ups := s.upstreams
	s.upstreams = map[string]*upstream{}
	s.mu.Unlock()
	for _, up := range ups {
		utilnet.CloseIdleConnectionsFor(up.rt)
	}
}

// upstreamDialer opens tugd's connections to clusters. It probes idle
// connections often, so that one whose network has gone (a VPN that
// dropped, another Wi-Fi network) fails within about half a minute rather
// than the several minutes TCP's defaults take, and the client sees its
// watch or stream end and starts again.
var upstreamDialer = &net.Dialer{
	Timeout:         30 * time.Second,
	KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3},
}

func (s *Server) newUpstream(clusterID string, cfg *rest.Config) (*upstream, error) {
	target, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing cluster address %q: %w", cfg.Host, err)
	}
	cfg = rest.CopyConfig(cfg)
	// Exec, attach and port-forward upgrade the connection; HTTP/2 cannot
	// carry SPDY upgrades, so keep the upstream on HTTP/1.1.
	cfg.NextProtos = []string{"http/1.1"}
	if cfg.Dial == nil {
		cfg.Dial = upstreamDialer.DialContext
	}
	rt, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, err
	}
	up := &upstream{rt: rt, created: s.cfg.Now()}
	up.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		Transport: rt,
		// Flush at once so watches and logs -f stream.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			pr, _ := resp.Request.Context().Value(proxyRequestKey{}).(*proxyRequest)
			if resp.StatusCode == http.StatusUnauthorized && pr != nil {
				explainRefusedCredential(resp, pr.sess.State.ClusterName, s.credentialRefused(clusterID, up))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			name := "the cluster"
			if pr, ok := r.Context().Value(proxyRequestKey{}).(*proxyRequest); ok {
				name = pr.sess.State.ClusterName
			}
			writeStatus(w, http.StatusBadGateway, metav1.StatusReasonServiceUnavailable,
				fmt.Sprintf("tug proxy: cannot reach %s: %v", name, err))
		},
	}
	return up, nil
}

// explainRefusedCredential replaces the body of a cluster's 401 with a
// Status that says it was tug's credential the cluster refused, not the
// session's token, and what happens next: whether tugd will ask for a new
// one. kubectl prints the message.
func explainRefusedCredential(resp *http.Response, cluster string, renewing bool) {
	_ = resp.Body.Close()
	next := "tugd asks its provider for a new one for the next request. If this happens again, run `tug doctor` to check the credential"
	if !renewing {
		next = "tugd got it from its provider moments ago, so run `tug doctor` to check the credential"
	}
	body := statusBody(http.StatusUnauthorized, metav1.StatusReasonUnauthorized, fmt.Sprintf(
		"tug proxy: %s refused tug's credential for it, which may have expired or been revoked; %s", cluster, next))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.TransferEncoding = nil
	resp.Header.Del("Content-Encoding")
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

// writeStatus answers with a Kubernetes Status, which kubectl and other
// clients print as an error from the server.
func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(statusBody(code, reason, msg))
}

// statusBody is a Kubernetes Status in JSON.
func statusBody(code int, reason metav1.StatusReason, msg string) []byte {
	b, _ := json.Marshal(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Message:  msg,
		Reason:   reason,
		Code:     int32(code),
	})
	return append(b, '\n')
}
