package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/pkg/provider"
)

func TestError(t *testing.T) {
	gr := schema.GroupResource{Group: "cluster.x-k8s.io", Resource: "clusters"}
	tests := []struct {
		name  string
		err   error
		code  provider.Code
		hint  string
		retry int
	}{
		{"unauthorized", apierrors.NewUnauthorized("token expired"), provider.Unauthenticated, "tug doctor mgmt", 0},
		{"forbidden", apierrors.NewForbidden(gr, "", errors.New("no list")), provider.PermissionDenied, "", 0},
		{"not found", apierrors.NewNotFound(gr, "x"), provider.NotFound, "", 0},
		{"throttled", apierrors.NewTooManyRequests("slow down", 7), provider.RateLimited, "", 7},
		{"unavailable", apierrors.NewServiceUnavailable("restarting"), provider.Unavailable, "", 0},
		{"connection refused", fmt.Errorf("Get %q: %w", "https://10.0.0.1", errors.New("connection refused")), provider.Unavailable, "", 0},
		{"cancelled", context.Canceled, provider.Unavailable, "", 0},
		{"provider error", provider.Errorf(provider.Unauthenticated, "sso expired").WithHint("aws sso login"), provider.Unauthenticated, "aws sso login", 0},
	}
	c := &Cluster{Name: "mgmt"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := provider.AsError(c.Error(tt.err, "listing clusters"))
			if pe.Code != tt.code || !strings.Contains(pe.Hint, tt.hint) || pe.RetryAfterSeconds != tt.retry ||
				!strings.HasPrefix(pe.Message, "listing clusters on mgmt: ") {
				t.Fatalf("Error(%v) = %+v, want %s with hint %q and retry %d", tt.err, pe, tt.code, tt.hint, tt.retry)
			}
		})
	}
	if c.Error(nil, "x") != nil {
		t.Fatal("Error(nil) is not nil")
	}
}

func TestConnect(t *testing.T) {
	f := func(_ context.Context, name string) (*rest.Config, error) {
		if name == "gone" {
			return nil, provider.Errorf(provider.NotFound, "gone is not in the catalog").WithHint("import it")
		}
		return &rest.Config{Host: "https://" + name}, nil
	}
	tests := []struct {
		name string
		f    ClusterConfig
		acct provider.Account
		code provider.Code
		hint string
	}{
		{"no setting", f, provider.Account{ID: "a"}, provider.InvalidConfig, "tug import capi --account a --set cluster="},
		{"no account", f, provider.Account{}, provider.InvalidConfig, "--account <account>"},
		{"unknown cluster", f, provider.Account{ID: "a", Config: map[string]any{ConfigCluster: "gone"}}, provider.NotFound, "import it"},
		{"not wired", nil, provider.Account{ID: "a", Config: map[string]any{ConfigCluster: "mgmt"}}, provider.Internal, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Connect(context.Background(), tt.f, "capi", tt.acct)
			if pe := provider.AsError(err); pe.Code != tt.code || !strings.Contains(pe.Hint, tt.hint) {
				t.Fatalf("Connect() error = %+v, want %s with hint %q", pe, tt.code, tt.hint)
			}
		})
	}
	mc, err := Connect(context.Background(), f, "capi", provider.Account{ID: "a", Config: map[string]any{ConfigCluster: "mgmt"}})
	if err != nil || mc.Name != "mgmt" || mc.Config.Host != "https://mgmt" || mc.Config.WarningHandler == nil {
		t.Fatalf("Connect() = %+v, %v", mc, err)
	}
}

func TestServedVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/example.com" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"kind":"APIGroup","apiVersion":"v1","name":"example.com",
			"versions":[{"groupVersion":"example.com/v2","version":"v2"},{"groupVersion":"example.com/v1","version":"v1"}]}`)
	}))
	t.Cleanup(srv.Close)
	c := &Cluster{Name: "mgmt", Config: &rest.Config{Host: srv.URL}}
	tests := []struct {
		group    string
		versions []string
		want     string
		code     provider.Code
	}{
		{"example.com", []string{"v1", "v2"}, "v1", ""},
		{"example.com", []string{"v3", "v2"}, "v2", ""},
		{"example.com", []string{"v3"}, "", provider.Unimplemented},
		{"missing.example.com", []string{"v1"}, "", provider.NotFound},
	}
	for _, tt := range tests {
		got, err := c.ServedVersion(context.Background(), tt.group, tt.versions...)
		var code provider.Code
		if err != nil {
			code = provider.CodeOf(err)
		}
		if got != tt.want || code != tt.code {
			t.Errorf("ServedVersion(%s, %v) = %q, %v, want %q (%s)", tt.group, tt.versions, got, err, tt.want, tt.code)
		}
	}
}

func TestExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if got := Expiry(now, time.Hour, nil); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("Expiry(no cert) = %s, want %s", got, now.Add(time.Hour))
	}
	if got := Expiry(now, time.Hour, []byte("not PEM")); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("Expiry(bad cert) = %s, want %s", got, now.Add(time.Hour))
	}
}
