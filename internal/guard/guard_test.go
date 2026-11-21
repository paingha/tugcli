package guard

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
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
	}{
		{"read on read-only", Policy{ReadOnly: true}, get, true},
		{"write on read-write", Policy{}, del, true},
		{"write on read-only", Policy{ReadOnly: true}, del, false},
		{"write while unlocked", Policy{ReadOnly: true, UnlockedUntil: now.Add(time.Minute)}, del, true},
		{"write after unlock expired", Policy{ReadOnly: true, UnlockedUntil: now.Add(-time.Minute)}, del, false},
		{"write confirmed", Policy{ReadOnly: true, Confirm: func(string) bool { return true }}, del, true},
		{"write declined", Policy{ReadOnly: true, Confirm: func(string) bool { return false }}, del, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.policy
			p.Cluster = "prod-eu"
			p.Now = func() time.Time { return now }
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
