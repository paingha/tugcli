package apiclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
