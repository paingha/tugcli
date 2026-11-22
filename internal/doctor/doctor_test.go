package doctor

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/pkg/provider"
)

const token = "doctor-token"

// fakeAPI is a Kubernetes API server with a configurable clock and version.
type fakeAPI struct {
	srv *httptest.Server
	// base is the moment the check runs at, as config gives it, in whole
	// seconds as HTTP dates are. now is the server's clock, which a case
	// sets relative to base, so the skew the check finds does not depend
	// on how long the test takes.
	base    time.Time
	now     time.Time
	version string
	// versionStatus, when set, is the status /version answers with.
	versionStatus int
	// reviewDelay stalls the self-review endpoints, until the client gives up.
	reviewDelay time.Duration
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	base := time.Now().Truncate(time.Second)
	f := &fakeAPI{base: base, now: base, version: "v1.35.1"}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", f.now.UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":401}`)
			return
		}
		if f.reviewDelay > 0 && strings.Contains(r.URL.Path, "/selfsubject") {
			select {
			case <-time.After(f.reviewDelay):
			case <-r.Context().Done():
				return
			}
		}
		switch r.URL.Path {
		case "/version":
			if f.versionStatus != 0 {
				w.WriteHeader(f.versionStatus)
				return
			}
			fmt.Fprintf(w, `{"gitVersion":%q}`, f.version)
		case "/apis/authentication.k8s.io/v1/selfsubjectreviews":
			fmt.Fprint(w, `{"kind":"SelfSubjectReview","apiVersion":"authentication.k8s.io/v1","status":{"userInfo":{"username":"alice","groups":["devs","system:authenticated"]}}}`)
		case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			// client-go sends built-in types as protobuf; decode with its codecs.
			b, _ := io.ReadAll(r.Body)
			obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(b, nil, nil)
			review, ok := obj.(*authzv1.SelfSubjectAccessReview)
			if err != nil || !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			verb := review.Spec.ResourceAttributes.Verb
			allowed := verb == "list" || verb == "get"
			fmt.Fprintf(w, `{"kind":"SelfSubjectAccessReview","apiVersion":"authorization.k8s.io/v1","status":{"allowed":%t}}`, allowed)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// The TCP check connects without TLS; keep the server's complaint quiet.
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) caPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
}

// config checks a healthy cluster served by f, with kubectl 1.35.
func (f *fakeAPI) config(t *testing.T) Config {
	t.Helper()
	cl := &catalog.Cluster{ID: "kubeconfig/x/dev", Name: "dev", Server: f.srv.URL, CAData: f.caPEM()}
	exp := f.base.Add(14 * time.Minute)
	return Config{
		Now:     func() time.Time { return f.base },
		Session: SessionInfo{ID: "abc123", Cluster: "dev", Kubeconfig: "/run/k", Env: "/run/k"},
		Store:   StoreInfo{Name: "memory"},
		Kubectl: KubectlInfo{Path: "/usr/bin/kubectl", Version: func(context.Context) (string, error) { return "v1.35.0", nil }},
		Kuberc:  KubercInfo{Path: "/home/me/.kube/kuberc", TugCommand: "/usr/local/bin/tug"},
		Cluster: cl,
		Credential: func(context.Context) (*credentials.Resolved, error) {
			return &credentials.Resolved{Token: token, ExpiresAt: &exp}, nil
		},
		RESTConfig: func(context.Context) (*rest.Config, error) {
			return &rest.Config{Host: f.srv.URL, BearerToken: token, TLSClientConfig: rest.TLSClientConfig{CAData: f.caPEM()}}, nil
		},
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	}
}

func byName(r Report) map[string]Result {
	m := map[string]Result{}
	for _, x := range r.Results {
		m[x.Name] = x
	}
	return m
}

func logReport(t *testing.T, r Report) {
	t.Helper()
	for _, x := range r.Results {
		t.Logf("%-4s %-28s %s | %s", x.Status, x.Name, x.Detail, x.Fix)
	}
}

func TestHealthyCluster(t *testing.T) {
	f := newFakeAPI(t)
	r := Run(context.Background(), f.config(t))
	if r.Failed() || r.Count(Warn) != 0 {
		logReport(t, r)
		t.Fatal("a healthy setup should have no failures or warnings")
	}
	got := byName(r)
	for name, want := range map[string]string{
		"TLS":                        "certificate verified",
		"credentials":                "bearer token, expires in",
		"API server":                 "Kubernetes v1.35.1",
		"clock":                      "in step",
		"version skew":               "kubectl v1.35.0 with server v1.35.1",
		"identity":                   "alice (groups: devs, system:authenticated)",
		"permissions":                "can list pods, logs; cannot exec, port-forward, patch deployments, delete pods",
		"kubectl":                    "v1.35.0 at /usr/bin/kubectl",
		"kubectl credential plugins": "no kuberc",
	} {
		if !strings.Contains(got[name].Detail, want) {
			t.Errorf("%s detail = %q, want it to contain %q", name, got[name].Detail, want)
		}
	}
	if _, ok := got["DNS"]; ok {
		t.Error("DNS should not be checked for an IP address")
	}
}

func TestClusterFailures(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*fakeAPI, *Config)
		check   string
		status  Status
		detail  string
		fixHint string
		absent  string // a later check that must not run
	}{
		{
			name: "unresolvable host",
			mutate: func(_ *fakeAPI, c *Config) {
				c.Cluster.Server = "https://api.cluster.invalid:6443"
				c.Resolver = fakeResolver{err: errors.New("no such host")}
			},
			check: "DNS", status: Fail, detail: "cannot resolve api.cluster.invalid", fixHint: "VPN", absent: "TCP",
		},
		{
			name: "unreachable",
			mutate: func(_ *fakeAPI, c *Config) {
				c.Dial = func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("connection timed out")
				}
			},
			check: "TCP", status: Fail, detail: "connection timed out", fixHint: "--egress-ip", absent: "credentials",
		},
		{
			name:   "wrong CA",
			mutate: func(_ *fakeAPI, c *Config) { c.Cluster.CAData = otherCA(t) },
			check:  "TLS", status: Fail, detail: "not signed by the CA", fixHint: "tug sync",
			absent: "credentials", // every later check needs TLS
		},
		{
			name:   "API server down",
			mutate: func(f *fakeAPI, _ *Config) { f.versionStatus = http.StatusServiceUnavailable },
			check:  "API server", status: Fail, detail: "503", fixHint: "try again", absent: "identity",
		},
		{
			name:   "version forbidden",
			mutate: func(f *fakeAPI, _ *Config) { f.versionStatus = http.StatusForbidden },
			check:  "API server", status: Warn, detail: "may not read /version",
		},
		{
			// A blocking ACL makes TCP fail; its finding must still appear.
			name: "access list checked before the network",
			mutate: func(_ *fakeAPI, c *Config) {
				c.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("i/o timeout") }
				c.Access = func(context.Context) ([]provider.Finding, error) {
					return []provider.Finding{{Severity: provider.SeverityError, Message: "ACL does not allow 198.51.100.9", Fix: "add 198.51.100.9/32"}}, nil
				}
			},
			check: "provider access list", status: Fail, detail: "does not allow", fixHint: "198.51.100.9/32", absent: "TLS",
		},
		{
			name:   "verification off",
			mutate: func(_ *fakeAPI, c *Config) { c.Cluster.InsecureSkipTLSVerify = true },
			check:  "TLS", status: Warn, detail: "verification is turned off",
		},
		{
			name: "credential error keeps the provider's hint",
			mutate: func(_ *fakeAPI, c *Config) {
				c.Credential = func(context.Context) (*credentials.Resolved, error) {
					return nil, provider.Errorf(provider.Unauthenticated, "SSO session expired").WithHint("aws sso login --profile work")
				}
			},
			check: "credentials", status: Fail, detail: "SSO session expired", fixHint: "aws sso login", absent: "API server",
		},
		{
			name: "rejected token",
			mutate: func(f *fakeAPI, c *Config) {
				c.RESTConfig = func(context.Context) (*rest.Config, error) {
					return &rest.Config{Host: f.srv.URL, BearerToken: "stale", TLSClientConfig: rest.TLSClientConfig{CAData: f.caPEM()}}, nil
				}
			},
			check: "API server", status: Fail, detail: "rejected the credential", fixHint: "--refresh", absent: "identity",
		},
		{
			name:   "clock far off",
			mutate: func(f *fakeAPI, _ *Config) { f.now = f.base.Add(-10 * time.Minute) },
			check:  "clock", status: Fail, detail: "10m0s off", fixHint: "synchronisation",
		},
		{
			name:   "clock slightly off",
			mutate: func(f *fakeAPI, _ *Config) { f.now = f.base.Add(time.Minute) },
			check:  "clock", status: Warn, detail: "1m0s off the server's",
		},
		{
			name: "old kubectl",
			mutate: func(_ *fakeAPI, c *Config) {
				c.Kubectl.Version = func(context.Context) (string, error) { return "v1.31.4", nil }
			},
			check: "version skew", status: Warn, detail: "4 minor versions", fixHint: "kubectl 1.34 to 1.36",
		},
		{
			name: "access list blocks this address",
			mutate: func(_ *fakeAPI, c *Config) {
				c.Access = func(context.Context) ([]provider.Finding, error) {
					return []provider.Finding{{Severity: provider.SeverityError, Message: "ACL does not allow 198.51.100.9", Fix: "add 198.51.100.9/32"}}, nil
				}
			},
			check: "provider access list", status: Fail, detail: "does not allow", fixHint: "198.51.100.9/32",
		},
		{
			name: "through a proxy",
			mutate: func(_ *fakeAPI, c *Config) {
				c.Cluster.ProxyURL = "socks5://127.0.0.1:1080"
				c.Dial = func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("direct dial must not happen")
				}
			},
			check: "network", status: Skip, detail: "through a proxy", absent: "TCP",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAPI(t)
			cfg := f.config(t)
			tt.mutate(f, &cfg)
			r := Run(context.Background(), cfg)
			got, ok := byName(r)[tt.check]
			if !ok || got.Status != tt.status || !strings.Contains(got.Detail, tt.detail) || !strings.Contains(got.Fix, tt.fixHint) {
				logReport(t, r)
				t.Fatalf("%s = %+v, want %s with %q and fix %q", tt.check, got, tt.status, tt.detail, tt.fixHint)
			}
			if tt.absent != "" {
				if _, ran := byName(r)[tt.absent]; ran {
					t.Fatalf("%s ran after %s failed", tt.absent, tt.check)
				}
			}
		})
	}
}

type fakeResolver struct{ err error }

func (f fakeResolver) LookupHost(context.Context, string) ([]string, error) { return nil, f.err }

// otherCA returns a CA that did not sign the test server's certificate.
func otherCA(t *testing.T) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: selfSigned(t)})
}

func TestHTTPEndpointUsesPort80(t *testing.T) {
	var dialled string
	cfg := Config{
		Cluster: &catalog.Cluster{Name: "local", Server: "http://127.0.0.1"},
		Proxy:   func(*http.Request) (*url.URL, error) { return nil, nil },
		Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialled = addr
			client, server := net.Pipe()
			server.Close()
			return client, nil
		},
	}
	Run(context.Background(), cfg)
	if dialled != "127.0.0.1:80" {
		t.Fatalf("dialled %q, want 127.0.0.1:80", dialled)
	}
}

func TestTunnelWithItsOwnEndpoint(t *testing.T) {
	f := newFakeAPI(t)
	cfg := f.config(t)
	// The stored CA is the Teleport proxy's, which the tunnel's local proxy
	// does not use, and the stored credential is not what signs in.
	cfg.Cluster.CAData = otherCA(t)
	cfg.Tunnel = "tsh://teleport.example.com:443/root/dev"
	cfg.TunnelEndpoint = true
	cfg.Credential = func(context.Context) (*credentials.Resolved, error) {
		return nil, errors.New("the stored credential should not be checked")
	}
	r := Run(context.Background(), cfg)
	got := byName(r)
	if r.Failed() || got["TLS"].Status != Skip || got["credentials"].Status != Skip || got["API server"].Status != OK {
		logReport(t, r)
		t.Fatal("want the TLS and credential checks left to the API check, and no failures")
	}
}

func TestStalledReviewsDoNotHang(t *testing.T) {
	f := newFakeAPI(t)
	f.reviewDelay = 1500 * time.Millisecond // answers only after the 200ms request timeout
	cfg := f.config(t)
	cfg.Timeout = 100 * time.Millisecond
	start := time.Now()
	r := Run(context.Background(), cfg)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("doctor took %s with stalled reviews", elapsed)
	}
	if got := byName(r)["identity"]; got.Status != Warn {
		logReport(t, r)
		t.Fatalf("identity = %+v, want a warning after the timeout", got)
	}
}

func TestKuberc(t *testing.T) {
	dir := t.TempDir()
	tug := filepath.Join(dir, "tug")
	if err := os.WriteFile(tug, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lookPath := func(name string) (string, error) {
		if name == "tug" {
			return tug, nil
		}
		return "", errors.New("not found")
	}
	tests := []struct {
		name   string
		info   KubercInfo
		status Status
		detail string
	}{
		{"turned off", KubercInfo{Disabled: true}, OK, "turned off"},
		{"no file", KubercInfo{}, OK, "no kuberc"},
		{"allow all by default", KubercInfo{Data: []byte("apiVersion: kubectl.config.k8s.io/v1beta1\nkind: Preference\n")}, OK, "allows every"},
		{"deny all", KubercInfo{Data: []byte("credentialPluginPolicy: DenyAll\n")}, Fail, "denies every"},
		{"allowlist by name on PATH", KubercInfo{Data: []byte("credentialPluginPolicy: Allowlist\ncredentialPluginAllowlist:\n- command: tug\n")}, OK, "allows tug"},
		{"allowlist by full path", KubercInfo{Data: []byte("credentialPluginPolicy: Allowlist\ncredentialPluginAllowlist:\n- command: " + tug + "\n")}, OK, "allows"},
		{"allowlist with the 1.35 name key", KubercInfo{Data: []byte("credentialPluginPolicy: Allowlist\ncredentialPluginAllowlist:\n- name: tug\n")}, OK, "allows tug"},
		{"allowlist without tug", KubercInfo{Data: []byte("credentialPluginPolicy: Allowlist\ncredentialPluginAllowlist:\n- command: aws\n")}, Fail, "does not include tug"},
		{"unknown policy", KubercInfo{Data: []byte("credentialPluginPolicy: Sometimes\n")}, Warn, "unknown"},
		{"not YAML", KubercInfo{Data: []byte("{{{")}, Warn, "not valid YAML"},
		{"unreadable", KubercInfo{ReadErr: errors.New("permission denied")}, Warn, "permission denied"},
		// kubectl will not start at all when $KUBERC names a missing file.
		{"explicit and missing", KubercInfo{Explicit: true, ReadErr: os.ErrNotExist}, Fail, "kubectl refuses to run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.info.Path = "/home/me/.kube/kuberc"
			tt.info.TugCommand = tug
			tt.info.LookPath = lookPath
			got := checkKuberc(tt.info)
			if got.Status != tt.status || !strings.Contains(got.Detail, tt.detail) {
				t.Fatalf("checkKuberc = %+v, want %s with %q", got, tt.status, tt.detail)
			}
			if got.Status == Fail && !tt.info.Explicit && !strings.Contains(got.Fix, "command: "+tug) {
				t.Fatalf("fix should name tug's path: %q", got.Fix)
			}
		})
	}
}

func TestSetupChecks(t *testing.T) {
	tests := []struct {
		name   string
		got    Result
		status Status
		detail string
	}{
		{"no hook", checkSession(SessionInfo{Shell: "bash"}), Warn, "no tug session"},
		{"ended session", checkSession(SessionInfo{ID: "abc", Err: errors.New("session abc has ended")}), Warn, "ended"},
		{"not connected", checkSession(SessionInfo{ID: "abc"}), OK, "not connected"},
		{"KUBECONFIG elsewhere", checkSession(SessionInfo{ID: "abc", Cluster: "dev", Kubeconfig: "/run/k", Env: "/home/me/.kube/config"}), Warn, "points elsewhere"},
		{"read-only", checkSession(SessionInfo{ID: "abc", Cluster: "prod", ReadOnly: true, Kubeconfig: "/run/k", Env: "/run/k"}), OK, "prod (read-only)"},
		{"no store", checkStore(StoreInfo{Err: errors.New("no keychain available")}), Fail, "no keychain"},
		// Opening the file store does not read it; the probe does.
		{"unreadable store", checkStore(StoreInfo{Name: "file:/x", Probe: func() error { return errors.New("cannot decrypt") }}), Fail, "cannot be read"},
		{"readable store", checkStore(StoreInfo{Name: "file:/x", Probe: func() error { return nil }}), OK, "file:/x"},
		{"no kubectl", func() Result { r, _ := checkKubectl(context.Background(), KubectlInfo{}); return r }(), Warn, "not on PATH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got.Status != tt.status || !strings.Contains(tt.got.Detail, tt.detail) {
				t.Fatalf("got %+v, want %s with %q", tt.got, tt.status, tt.detail)
			}
		})
	}
	for shell, want := range map[string]string{
		"zsh":        `eval "$(tug init zsh)"`,
		"fish":       "tug init fish | source",
		"powershell": "Invoke-Expression (& tug init powershell | Out-String)",
	} {
		if r := checkSession(SessionInfo{Shell: shell}); !strings.Contains(r.Fix, want) {
			t.Errorf("%s fix = %q, want %q", shell, r.Fix, want)
		}
	}
}

func TestCatalogCheck(t *testing.T) {
	if r := checkCatalog(&catalog.Catalog{}); r[0].Status != Warn || !strings.Contains(r[0].Fix, "tug import") {
		t.Fatalf("empty catalog: %+v", r)
	}
	c := &catalog.Catalog{
		Sources:  []catalog.Source{{Provider: "aws", Account: "work", LastError: "Unauthenticated: SSO session expired"}},
		Clusters: []catalog.Cluster{{Name: "a"}, {Name: "b", Stale: true}},
	}
	r := checkCatalog(c)
	if len(r) != 2 || r[0].Status != Warn || !strings.Contains(r[0].Detail, "1 no longer listed") ||
		r[1].Name != "source aws/work" || !strings.Contains(r[1].Detail, "SSO session expired") {
		t.Fatalf("catalog with problems: %+v", r)
	}
}

func TestMinor(t *testing.T) {
	for in, want := range map[string]int{"v1.35.1": 35, "1.34": 34, "v1.33.5-gke.100": 33, "v1.36+": 36} {
		if got, ok := minor(in); !ok || got != want {
			t.Errorf("minor(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	if _, ok := minor("v2.0.0"); ok {
		t.Error("minor should reject non-1.x versions")
	}
}
