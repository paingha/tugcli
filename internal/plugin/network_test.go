package plugin

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseHostPattern(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr string
	}{
		{"api.example.com", "api.example.com", ""},
		{"API.Example.com", "api.example.com", ""},
		{"api.example.com:443", "api.example.com", ""},
		{"api.example.com:8443", "api.example.com:8443", ""},
		{"*.example.com", "*.example.com", ""},
		{"127.0.0.1:6443", "127.0.0.1:6443", ""},
		{"[2001:db8::1]:8443", "[2001:db8::1]:8443", ""},
		{"2001:db8::1", "[2001:db8::1]", ""},
		{"[::1]", "[::1]", ""},
		{"https://api.example.com", "", "without a scheme or path"},
		{"api.example.com/v1", "", "without a scheme or path"},
		{"the Rancher server in the account's url setting", "", "not a host name"},
		{"api.example.com:http", "", "not a number"},
		{"api.example.com:0", "", "not a number"},
		{"*.10.0.0.1", "", "needs a domain name"},
		{"-bad.example.com", "", "not a host name"},
		{"", "", "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			p, err := parseHostPattern(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseHostPattern(%q) = %v, %v; want an error containing %q", tt.in, p, err, tt.wantErr)
				}
				return
			}
			if err != nil || p.String() != tt.want {
				t.Fatalf("parseHostPattern(%q) = %v, %v; want %s", tt.in, p, err, tt.want)
			}
		})
	}
}

func TestAllowListCheck(t *testing.T) {
	l, err := newAllowList([]string{"api.example.com", "*.cloud.example", "10.0.0.1:6443", "[2001:db8::1]"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		url     string
		wantErr string
	}{
		{"https://api.example.com/v1/clusters?page=2", ""},
		{"https://API.EXAMPLE.COM./v1", ""},
		{"https://api.example.com:443/", ""},
		{"https://eu.cloud.example/", ""},
		{"https://a.b.cloud.example/", ""},
		{"https://10.0.0.1:6443/", ""},
		{"https://[2001:db8::1]/", ""},
		{"http://api.example.com/", "is not https"},
		{"https://api.example.com:8443/", "api.example.com:8443 is not in the plugin's spec.network"},
		{"https://cloud.example/", "cloud.example is not in"},
		{"https://evil.com/", "evil.com is not in"},
		{"https://api.example.com.evil.com/", "is not in"},
		{"https://10.0.0.1/", "10.0.0.1 is not in"},
		{"https://user:pass@evil.com/", "evil.com is not in"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			u, err := url.Parse(tt.url)
			if err != nil {
				t.Fatal(err)
			}
			err = l.check(u)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("check(%s) = %v, want nil", tt.url, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("check(%s) = %v, want an error containing %q", tt.url, err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "pass") {
				t.Fatalf("check(%s) = %v, which shows the URL's password", tt.url, err)
			}
		})
	}
	var empty allowList
	if err := empty.check(&url.URL{Scheme: "https", Host: "api.example.com"}); err == nil || !strings.Contains(err.Error(), "lists no hosts") {
		t.Fatalf("an empty spec.network allowed a request: %v", err)
	}
}
