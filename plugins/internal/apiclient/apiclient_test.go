package apiclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paingha/tugcli/pkg/provider"
)

func TestBaseURL(t *testing.T) {
	tests := []struct {
		apiURL  string
		want    string
		wantErr bool
	}{
		{"", "https://default", false},
		{"https://eu.api.example.com/v4/", "https://eu.api.example.com/v4", false},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"http://localhost:8080/v4", "http://localhost:8080/v4", false},
		{"http://api.example.com", "", true},
		{"not a url", "", true},
	}
	for _, tt := range tests {
		got, err := BaseURL(provider.Account{Config: map[string]any{ConfigAPIURL: tt.apiURL}}, "https://default")
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("BaseURL(%q) = %q, %v", tt.apiURL, got, err)
		}
	}
}

func TestCheckURL(t *testing.T) {
	if got, err := CheckURL("url", "https://rancher.example.com/"); err != nil || got != "https://rancher.example.com" {
		t.Errorf(`CheckURL("url", "https://rancher.example.com/") = %q, %v, want "https://rancher.example.com"`, got, err)
	}
	_, err := CheckURL("url", "http://rancher.example.com")
	if pe := provider.AsError(err); pe.Code != provider.InvalidConfig || pe.Message != "url must use https" {
		t.Errorf(`CheckURL("url", "http://rancher.example.com") = %+v, want InvalidConfig naming the setting`, pe)
	}
}

func TestDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer tok":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"why":"bad token"}`)
		case r.URL.Path == "/slow":
			w.Header().Set("Retry-After", "12")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			fmt.Fprint(w, `{"name":"ok"}`)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok", Message: func(b []byte) string { return "parsed: " + string(b) }}

	var out struct{ Name string }
	if err := c.Do(context.Background(), http.MethodGet, "/x", nil, &out); err != nil || out.Name != "ok" {
		t.Fatalf("Do = %+v, %v", out, err)
	}
	err := c.Do(context.Background(), http.MethodGet, "/slow", nil, nil)
	if pe := provider.AsError(err); pe.Code != provider.RateLimited || pe.RetryAfterSeconds != 12 {
		t.Fatalf("429: %+v", pe)
	}
	bad := &Client{BaseURL: srv.URL, Token: "nope", Message: c.Message}
	err = bad.Do(context.Background(), http.MethodGet, "/x", nil, nil)
	if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || pe.Message != `GET /x: parsed: {"why":"bad token"}` {
		t.Fatalf("401: %+v", pe)
	}
}

func TestDoSign(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Signed GET /v1/x?a=1"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1", Token: "unused", Sign: func(req *http.Request) error {
		req.Header.Set("Authorization", "Signed "+req.Method+" "+req.URL.RequestURI())
		return nil
	}}
	if err := c.Do(context.Background(), http.MethodGet, "/x?a=1", nil, nil); err != nil {
		t.Fatal(err)
	}

	failing := &Client{BaseURL: srv.URL, Sign: func(*http.Request) error {
		return provider.Errorf(provider.Unauthenticated, "no key")
	}}
	if err := failing.Do(context.Background(), http.MethodGet, "/x", nil, nil); provider.CodeOf(err) != provider.Unauthenticated {
		t.Fatalf("Do with a failing Sign = %v, want its error", err)
	}
}

func TestSend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/form":
			if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" || string(body) != "a=1" {
				t.Errorf("form request has Content-Type %q and body %q", ct, body)
			}
			w.Header().Set("Next-Page", "p2")
			fmt.Fprint(w, "kind: Config\n")
		case "/missing":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"errorCode":"INVALID_KEY"}`)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	code := func(status int, body []byte) provider.Code {
		if status == http.StatusForbidden && strings.Contains(string(body), "INVALID_KEY") {
			return provider.Unauthenticated
		}
		return ""
	}
	c := &Client{BaseURL: srv.URL, Token: "tok", Code: code}
	ctx := context.Background()

	form := RawBody{ContentType: "application/x-www-form-urlencoded", Data: []byte("a=1")}
	resp, err := c.Send(ctx, http.MethodPost, "/form", form)
	if err != nil || string(resp.Body) != "kind: Config\n" || resp.Header.Get("Next-Page") != "p2" {
		t.Fatalf("Send(/form) = %+v, %v, want the body and headers", resp, err)
	}
	if _, err := c.Send(ctx, http.MethodGet, "/missing", nil); provider.CodeOf(err) != provider.Unauthenticated {
		t.Errorf("Send(/missing) = %v, want the Unauthenticated that Code picks", err)
	}
	if _, err := c.Send(ctx, http.MethodGet, "/other", nil); provider.CodeOf(err) != provider.PermissionDenied {
		t.Errorf("Send(/other) = %v, want the PermissionDenied of a 403", err)
	}
}
