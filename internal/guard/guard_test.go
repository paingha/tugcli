package guard

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/audit"
)

func TestIsRead(t *testing.T) {
	tests := []struct {
		method, url string
		want        bool
	}{
		{"GET", "/api/v1/namespaces/default/pods", true},
		{"GET", "/api/v1/namespaces/default/pods/api-0/log?follow=true", true},
		{"POST", "/api/v1/namespaces/default/pods/api-0/portforward", true},
		{"POST", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", true},
		{"POST", "/apis/apps/v1/namespaces/default/deployments?dryRun=All", true},
		{"GET", "/api/v1/namespaces/default/pods/api-0/exec?command=sh", false},
		{"POST", "/api/v1/namespaces/default/pods/api-0/exec", false},
		{"GET", "/api/v1/namespaces/default/pods/api-0/attach", false},
		{"DELETE", "/api/v1/namespaces/default/pods/api-0", false},
		{"PATCH", "/apis/apps/v1/namespaces/default/deployments/api/scale", false},
		{"POST", "/api/v1/namespaces", false},

		// The kubelet's exec and attach are WebSocket GETs through the API
		// server's node proxy; they run code, so they are writes.
		{"GET", "/api/v1/nodes/n1/proxy/exec/default/api-0/app?command=sh&input=1", false},
		{"GET", "/api/v1/nodes/n1:10250/proxy/attach/default/api-0/app", false},
		{"GET", "/api/v1/nodes/n1/proxy/Exec/default/api-0/app", false},
		{"POST", "/api/v1/nodes/n1/proxy/run/default/api-0/app", false},
		{"POST", "/api/v1/nodes/n1/proxy/checkpoint/default/api-0/app", false},
		{"GET", "/api/v1/nodes/n1/proxy/stats/summary", true},
		{"GET", "/api/v1/nodes/n1/proxy/containerLogs/default/api-0/app", true},
		{"GET", "/api/v1/nodes/n1", true},

		// Paths a client would not send are not reads, whatever the
		// server would make of them.
		{"GET", "/api/v1/namespaces/default/pods/api-0/exec//", false},
		{"GET", "/api/v1/namespaces/default/pods/api-0/exec/.", false},
		{"GET", "/api/v1/namespaces/default/pods/api-0/exec/../log", false},
		{"GET", "/api/v1/nodes/n1/proxy//exec/default/api-0/app", false},
		{"GET", "/", true},
		{"GET", "/api/", true},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.url, func(t *testing.T) {
			u, _ := url.Parse(tt.url)
			if got := IsRead(tt.method, u); got != tt.want {
				t.Errorf("IsRead = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	u, _ := url.Parse("/apis/apps/v1/namespaces/web/deployments/api/scale")
	if got := Describe("PATCH", u); got != "PATCH deployments/api/scale" {
		t.Errorf("Describe = %q", got)
	}
}

type okTransport struct{ calls int }

func (o *okTransport) RoundTrip(*http.Request) (*http.Response, error) {
	o.calls++
	return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
}

func TestRoundTripper(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	del, _ := http.NewRequest(http.MethodDelete, "https://k8s/api/v1/namespaces/default/pods/x", nil)
	get, _ := http.NewRequest(http.MethodGet, "https://k8s/api/v1/namespaces/default/pods", nil)

	tests := []struct {
		name    string
		policy  Policy
		req     *http.Request
		allowed bool
		// recorded is how the audit entry says the write was allowed, or ""
		// when nothing should be recorded.
		recorded string
	}{
		{"read on read-only", Policy{ReadOnly: true}, get, true, ""},
		{"write on read-write", Policy{}, del, true, audit.AllowedReadWrite},
		{"write on read-only", Policy{ReadOnly: true}, del, false, ""},
		{"write while unlocked", Policy{ReadOnly: true, UnlockedUntil: now.Add(time.Minute)}, del, true, audit.AllowedUnlocked},
		{"write after unlock expired", Policy{ReadOnly: true, UnlockedUntil: now.Add(-time.Minute)}, del, false, ""},
		{"write confirmed", Policy{ReadOnly: true, Confirm: func(string) bool { return true }}, del, true, audit.AllowedConfirmed},
		{"write declined", Policy{ReadOnly: true, Confirm: func(string) bool { return false }}, del, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.policy
			p.Cluster, p.ClusterID, p.Session = "prod-eu", "aws/1/prod-eu", "a1b2c3d4e5f6"
			p.Now = func() time.Time { return now }
			rec := &recorder{}
			p.Audit = rec
			next := &okTransport{}
			resp, err := Wrap(next, &p).RoundTrip(tt.req)
			if resp != nil {
				resp.Body.Close()
			}
			if tt.allowed && (err != nil || next.calls != 1) {
				t.Fatalf("want allowed, got %v (calls %d)", err, next.calls)
			}
			var denied *DeniedError
			if !tt.allowed && (!errors.As(err, &denied) || next.calls != 0) {
				t.Fatalf("want DeniedError, got %v (calls %d)", err, next.calls)
			}
			if tt.recorded == "" {
				if len(rec.entries) != 0 {
					t.Fatalf("recorded %+v, want nothing", rec.entries)
				}
				return
			}
			want := audit.Entry{
				Time: now, Source: audit.SourceTug, Cluster: "prod-eu", ClusterID: "aws/1/prod-eu", Session: "a1b2c3d4e5f6",
				Allowed: tt.recorded, Method: http.MethodDelete, Path: "/api/v1/namespaces/default/pods/x",
			}
			if len(rec.entries) != 1 || !reflect.DeepEqual(rec.entries[0], want) {
				t.Fatalf("recorded %+v, want %+v", rec.entries, want)
			}
		})
	}
}

func TestRoundTripperBelowABasePath(t *testing.T) {
	const base = "/k8s/clusters/c-1"
	tests := []struct {
		method, path string
		allowed      bool
		action       string
	}{
		{http.MethodPost, base + "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", true, ""},
		{http.MethodDelete, base + "/api/v1/namespaces/default/pods/x", false, "DELETE pods/x"},
		{http.MethodPost, base + "/api/v1/namespaces/default/pods/x/exec", false, "POST pods/x/exec"},
		// A path that only starts with the same characters is not below
		// the base.
		{http.MethodPost, base + "0/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", false, "POST k8s/clusters/c-10/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, "https://rancher.example.com"+tt.path, nil)
			next := &okTransport{}
			resp, err := WrapBase(next, &Policy{Cluster: "prod-eu", ReadOnly: true}, base+"/").RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
			var denied *DeniedError
			switch {
			case tt.allowed && (err != nil || next.calls != 1):
				t.Fatalf("RoundTrip = %v (calls %d), want it allowed", err, next.calls)
			case !tt.allowed && (!errors.As(err, &denied) || denied.Action != tt.action || next.calls != 0):
				t.Fatalf("RoundTrip = %v (calls %d), want %q denied", err, next.calls, tt.action)
			}
		})
	}
}

// recorder keeps the entries it is given, or fails with err.
type recorder struct {
	entries []audit.Entry
	err     error
}

func (r *recorder) Record(_ context.Context, e audit.Entry) error {
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, e)
	return nil
}

// A write that cannot be recorded is not sent, and the query string, which
// can carry an exec's command, is never recorded.
func TestWritesAreRecordedOrNotSent(t *testing.T) {
	exec, _ := http.NewRequest(http.MethodPost, "https://k8s/api/v1/namespaces/default/pods/x/exec?command=env&command=PASSWORD%3Dhunter2", nil)
	rec := &recorder{}
	next := &okTransport{}
	resp, err := Wrap(next, &Policy{Cluster: "dev", Audit: rec}).RoundTrip(exec)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(rec.entries) != 1 || rec.entries[0].Path != "/api/v1/namespaces/default/pods/x/exec" {
		t.Fatalf("recorded %+v, want the path without its query", rec.entries)
	}

	full := errors.New("no space left on device")
	next = &okTransport{}
	resp, err = Wrap(next, &Policy{Cluster: "dev", Audit: &recorder{err: full}}).RoundTrip(exec)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, full) || !strings.Contains(err.Error(), "not sending POST pods/x/exec") || next.calls != 0 {
		t.Fatalf("RoundTrip with a failing audit log = %v (calls %d), want the write refused", err, next.calls)
	}

	// Below a base path, the entry has the Kubernetes API path.
	del, _ := http.NewRequest(http.MethodDelete, "https://rancher.example.com/k8s/clusters/c-1/api/v1/namespaces/default/pods/x", nil)
	rec = &recorder{}
	resp, err = WrapBase(&okTransport{}, &Policy{Cluster: "dev", Audit: rec}, "/k8s/clusters/c-1").RoundTrip(del)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(rec.entries) != 1 || rec.entries[0].Path != "/api/v1/namespaces/default/pods/x" {
		t.Fatalf("recorded %+v, want the path below the base", rec.entries)
	}
}

func TestConfirmAskedOncePerCommand(t *testing.T) {
	asked := 0
	p := &Policy{ReadOnly: true, Confirm: func(string) bool { asked++; return true }}
	rt := Wrap(&okTransport{}, p)
	for range 3 {
		req, _ := http.NewRequest(http.MethodDelete, "https://k8s/api/v1/namespaces/default/pods/x", nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if asked != 1 {
		t.Fatalf("asked %d times", asked)
	}
}

func TestReadOnlyFor(t *testing.T) {
	sel := []map[string]string{{"env": "prod"}, {"tier": "critical", "region": "eu"}}
	tests := []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{"env": "prod"}, true},
		{map[string]string{"env": "staging"}, false},
		{map[string]string{"tier": "critical"}, false},
		{map[string]string{"tier": "critical", "region": "eu"}, true},
	}
	for _, tt := range tests {
		if got := ReadOnlyFor(tt.labels, sel); got != tt.want {
			t.Errorf("ReadOnlyFor(%v) = %v", tt.labels, got)
		}
	}
}
