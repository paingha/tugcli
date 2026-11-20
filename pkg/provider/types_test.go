package provider

import (
	"strings"
	"testing"
	"time"
)

func TestCredentialValidate(t *testing.T) {
	tests := []struct {
		name    string
		c       Credential
		wantErr string // "" means valid
	}{
		{"token", Token("t", NoExpiry), ""},
		{"token without token", Credential{Type: CredentialToken}, "no token"},
		{"client cert", ClientCert([]byte("C"), []byte("K"), NoExpiry), ""},
		{"client cert without key", ClientCert([]byte("C"), nil, NoExpiry), "certData and keyData"},
		{"client cert without cert", ClientCert(nil, []byte("K"), NoExpiry), "certData and keyData"},
		{"oidc", OIDC("https://issuer", "tug", "openid"), ""},
		{"oidc without client ID", OIDC("https://issuer", ""), "issuer and clientID"},
		{"oidc without issuer", OIDC("", "tug"), "issuer and clientID"},
		{"delegate", Delegate("aws", "eks", "get-token"), ""},
		{"delegate without exec", Credential{Type: CredentialDelegate}, "exec.command"},
		{"delegate without command", Delegate(""), "exec.command"},
		{"unknown type", Credential{Type: "password"}, `"password"`},
		{"no type", Credential{Token: "t"}, "unknown credential type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCredentialExpired(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	const skew = time.Minute
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"no expiry", NoExpiry, false},
		{"well before expiry", now.Add(time.Hour), false},
		{"just outside the skew", now.Add(skew + time.Second), false},
		{"exactly at the skew", now.Add(skew), true},
		{"within the skew", now.Add(30 * time.Second), true},
		{"already expired", now.Add(-time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Token("t", tt.expiresAt).Expired(now, skew); got != tt.want {
				t.Fatalf("Expired(now, %v) with expiry %v = %v, want %v", skew, tt.expiresAt, got, tt.want)
			}
		})
	}
}

func TestConstructorsNormaliseExpiry(t *testing.T) {
	if c := Token("t", NoExpiry); c.ExpiresAt != nil {
		t.Fatalf("Token(NoExpiry).ExpiresAt = %v, want nil", c.ExpiresAt)
	}
	local := time.Date(2026, 11, 20, 13, 0, 0, 0, time.FixedZone("CET", 3600))
	c := ClientCert([]byte("C"), []byte("K"), local)
	if c.ExpiresAt == nil || c.ExpiresAt.Location() != time.UTC || !c.ExpiresAt.Equal(local) {
		t.Fatalf("ClientCert expiry = %v, want %v in UTC", c.ExpiresAt, local)
	}
}
