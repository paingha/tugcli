package iap

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseRelay(t *testing.T) {
	tests := []struct {
		in, want, wantErr string
	}{
		{in: DefaultRelayURL, want: "wss://tunnel.cloudproxy.app/v4"},
		{in: "wss://relay.example", want: "wss://relay.example/v4"},
		{in: "wss://relay.example/", want: "wss://relay.example/v4"},
		{in: "wss://relay.example/v9?x=1", want: "wss://relay.example/v9"},
		{in: "ws://127.0.0.1:8080", want: "ws://127.0.0.1:8080/v4"},
		{in: "ws://relay.example", wantErr: "must use wss"},
		{in: "https://relay.example", wantErr: "must use wss"},
		{in: "wss:///v4", wantErr: "has no host"},
	}
	for _, tt := range tests {
		u, err := parseRelay(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("parseRelay(%q) error = %v, want one containing %q", tt.in, err, tt.wantErr)
			}
			continue
		}
		if err != nil || u.String() != tt.want {
			t.Errorf("parseRelay(%q) = %v, %v; want %s", tt.in, u, err, tt.want)
		}
	}
	cl := &client{relay: &url.URL{Scheme: "wss", Host: "tunnel.cloudproxy.app", Path: "/v4"}}
	if got, want := cl.endpoint("connect", url.Values{"a": {"b c"}}), "wss://tunnel.cloudproxy.app/v4/connect?a=b+c"; got != want {
		t.Errorf("endpoint() = %s, want %s", got, want)
	}
}
