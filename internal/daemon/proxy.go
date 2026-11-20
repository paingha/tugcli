package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/guard"
)

// upstream is a reverse proxy to one cluster's API server.
type upstream struct {
	proxy   *httputil.ReverseProxy
	created time.Time
}

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
	sess, err := s.cfg.Sessions.Open(sessionID)
	if err != nil || !sess.State.Connected() {
		writeStatus(w, http.StatusGone, metav1.StatusReasonGone, "tug proxy: this tug session has ended or is not connected; run `tug connect`")
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
		writeStatus(w, http.StatusBadGateway, metav1.StatusReasonServiceUnavailable,
			fmt.Sprintf("tug proxy: cannot reach %s: %v", sess.State.ClusterName, err))
		return
	}
	out := r.Clone(r.Context())
	out.URL.Path, out.URL.RawPath = rest, ""
	// The session token is for tugd only; the upstream gets the cluster's
	// own credential from the transport.
	out.Header.Del("Authorization")
	up.proxy.ServeHTTP(w, out)
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
// there is none or it is older than upstreamTTL.
func (s *Server) upstream(ctx context.Context, clusterID string) (*upstream, error) {
	s.mu.Lock()
	up, ok := s.upstreams[clusterID]
	s.mu.Unlock()
	if ok && s.cfg.Now().Sub(up.created) < upstreamTTL {
		return up, nil
	}
	cfg, err := s.cfg.Upstream(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	up, err = newUpstream(cfg, s.cfg.Now())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.upstreams[clusterID] = up
	s.mu.Unlock()
	return up, nil
}

func newUpstream(cfg *rest.Config, now time.Time) (*upstream, error) {
	target, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing cluster address %q: %w", cfg.Host, err)
	}
	cfg = rest.CopyConfig(cfg)
	// Exec, attach and port-forward upgrade the connection; HTTP/2 cannot
	// carry SPDY upgrades, so keep the upstream on HTTP/1.1.
	cfg.NextProtos = []string{"http/1.1"}
	rt, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, err
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		Transport: rt,
		// Flush at once so watches and logs -f stream.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeStatus(w, http.StatusBadGateway, metav1.StatusReasonServiceUnavailable, "tug proxy: "+err.Error())
		},
	}
	return &upstream{proxy: proxy, created: now}, nil
}

// writeStatus answers with a Kubernetes Status, which kubectl and other
// clients print as an error from the server.
func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Message:  msg,
		Reason:   reason,
		Code:     int32(code),
	})
}
