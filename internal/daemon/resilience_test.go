package daemon

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/kube"
	sdk "github.com/paingha/tugcli/pkg/provider"
)

// clock is a time that tests move.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// rotatingProvider hands out a new token on every fetch, each expiring at
// its time.
type rotatingProvider struct {
	mu      sync.Mutex
	tokens  []string
	expires []time.Time
	fetches int
}

func (p *rotatingProvider) fetch(context.Context, string) (sdk.Credential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fetches >= len(p.tokens) {
		return sdk.Credential{}, sdk.Errorf(sdk.Unauthenticated, "the provider has no more credentials").WithHint("sign in again")
	}
	exp := p.expires[p.fetches]
	c := sdk.Credential{Type: sdk.CredentialToken, Token: p.tokens[p.fetches], ExpiresAt: &exp}
	p.fetches++
	return c, nil
}

func (p *rotatingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetches
}

// Through tugd, an expiring token is replaced mid-session; a token the
// cluster refuses, as when it is revoked, is explained to the client and
// replaced from the provider for the next request.
func TestProxyRefreshesCredentialsMidSession(t *testing.T) {
	// The broker's clock, which says when a stored credential has expired.
	// It runs an hour behind, so client-go, which caches tokens by the real
	// clock, asks the broker every time.
	c := &clock{t: time.Now().Add(-time.Hour)}
	t0 := c.now()
	p := &rotatingProvider{
		tokens:  []string{"token-1", "token-2", "token-3"},
		expires: []time.Time{t0.Add(10 * time.Minute), t0.Add(2 * time.Hour), t0.Add(2 * time.Hour)},
	}
	broker := &credentials.Broker{Store: credentials.NewMemoryStore(), Fetch: p.fetch, Now: c.now}
	var cluster *fakeCluster
	var builds, refreshes atomic.Int32
	tugdClock := &clock{t: time.Now()}
	f := setup(t, func(cfg *Config) {
		cfg.Now = tugdClock.now
		cfg.Upstream = func(ctx context.Context, clusterID string, refresh bool) (*rest.Config, error) {
			builds.Add(1)
			if refresh {
				refreshes.Add(1)
			}
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.srv.Certificate().Raw})
			cl := &catalog.Cluster{ID: clusterID, Name: "prod", Server: cluster.srv.URL, CAData: ca}
			return (&kube.Factory{Cluster: cl, Broker: broker, Refresh: refresh}).RESTConfig(ctx)
		}
	})
	cluster = f.cluster
	cs := f.clientset(t)
	list := func() error {
		_, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
		return err
	}

	cluster.valid.Store("token-1")
	if err := list(); err != nil {
		t.Fatalf("list with the first token: %v", err)
	}
	// The token expires; the next request takes a new one.
	cluster.valid.Store("token-2")
	c.set(t0.Add(10 * time.Minute))
	if err := list(); err != nil {
		t.Fatalf("list once the first token expired: %v", err)
	}
	if n, b := p.count(), builds.Load(); n != 2 || b != 1 {
		t.Fatalf("the provider was asked %d times (want 2) and the upstream built %d (want 1)", n, b)
	}

	// The cluster stops taking token-2 before it expires.
	cluster.valid.Store("token-3")
	err := list()
	if !apierrors.IsUnauthorized(err) || !strings.Contains(err.Error(), "prod refused tug's credential") ||
		!strings.Contains(err.Error(), "tug doctor") {
		t.Fatalf("list with a revoked token: %v; want Unauthorized saying what to do", err)
	}
	if err := list(); err != nil {
		t.Fatalf("list after a revoked token: %v; want a new one from the provider", err)
	}
	if n, r := p.count(), refreshes.Load(); n != 3 || r != 1 {
		t.Fatalf("the provider was asked %d times (want 3) and the upstream built with refresh %d (want 1)", n, r)
	}

	// A credential the provider gave moments ago is not asked for again
	// at once, when the cluster refuses it too.
	cluster.valid.Store("token-4")
	for range 3 {
		if err := list(); !apierrors.IsUnauthorized(err) || !strings.Contains(err.Error(), "moments ago") {
			t.Fatalf("list with a new token refused: %v", err)
		}
	}
	if n := p.count(); n != 3 {
		t.Fatalf("the provider was asked %d times, want no more than 3", n)
	}
	// Later it is, and when the provider has nothing better, its error and
	// hint reach the client, as an authentication failure.
	tugdClock.set(tugdClock.now().Add(refreshBackoff))
	if err := list(); !apierrors.IsUnauthorized(err) || !strings.Contains(err.Error(), "asks its provider for a new one") {
		t.Fatalf("list with token-3 refused later: %v", err)
	}
	err = list()
	if !apierrors.IsUnauthorized(err) || !strings.Contains(err.Error(), "no more credentials") || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("list when the provider fails: %v; want its error and hint", err)
	}
}

// clientset is a client-go client for the fixture's session, as kubectl
// would use it.
func (f *fixture) clientset(t *testing.T) kubernetes.Interface {
	t.Helper()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: f.proxy.URL + "/s/" + f.sess.State.ID, BearerToken: f.token})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// A provider's hint for a credential it cannot give reaches the client.
func TestProxyPassesOnProviderHints(t *testing.T) {
	var buildErr atomic.Pointer[error]
	fail := func(err error) { buildErr.Store(&err) }
	f := setup(t, func(c *Config) {
		c.Upstream = func(context.Context, string, bool) (*rest.Config, error) {
			return nil, *buildErr.Load()
		}
	})
	fail(fmt.Errorf("getting credentials for prod: %w",
		sdk.Errorf(sdk.Unauthenticated, "not signed in to https://idp").WithHint("run `tug auth login prod` to sign in")))
	code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token)
	if code != http.StatusUnauthorized || !strings.Contains(body, "not signed in") || !strings.Contains(body, "tug auth login prod") {
		t.Fatalf("GET when the provider needs a sign-in = %d %s", code, body)
	}
	fail(errors.New("catalog: no such cluster"))
	if code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token); code != http.StatusBadGateway || !strings.Contains(body, "cannot forward to prod") {
		t.Fatalf("GET with a broken catalog = %d %s", code, body)
	}
}
