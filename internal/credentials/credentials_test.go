package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/paingha/tugcli/pkg/provider"
)

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	s := NewFileStore(path, "correct horse")

	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	if err := s.Set("a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("b", "2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}

	reopened := NewFileStore(path, "correct horse")
	if v, err := reopened.Get("b"); err != nil || v != "2" {
		t.Fatalf("Get(b) = %q, %v", v, err)
	}
	if _, err := reopened.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key still present: %v", err)
	}

	wrong := NewFileStore(path, "wrong")
	if _, err := wrong.Get("b"); err == nil || !strings.Contains(err.Error(), "cannot decrypt") {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

// fakeKeyring enforces a per-item size limit like the real backends.
type fakeKeyring struct {
	items map[string]string
	limit int
}

func (f *fakeKeyring) Get(_, user string) (string, error) {
	v, ok := f.items[user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Set(_, user, password string) error {
	if len(password) > f.limit {
		return keyring.ErrSetDataTooBig
	}
	f.items[user] = password
	return nil
}

func (f *fakeKeyring) Delete(_, user string) error {
	if _, ok := f.items[user]; !ok {
		return keyring.ErrNotFound
	}
	delete(f.items, user)
	return nil
}

func TestKeychainChunking(t *testing.T) {
	fk := &fakeKeyring{items: map[string]string{}, limit: 2000}
	k := &KeychainStore{kr: fk}

	big := strings.Repeat("x", 5000)
	if err := k.Set("cred/big", big); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get("cred/big"); err != nil || got != big {
		t.Fatalf("chunked round trip failed: len %d, %v", len(got), err)
	}
	if len(fk.items) != 4 { // header + 3 chunks
		t.Fatalf("want 4 items, got %d", len(fk.items))
	}

	// Replacing with a small value removes the old chunks.
	if err := k.Set("cred/big", "small"); err != nil {
		t.Fatal(err)
	}
	if len(fk.items) != 1 {
		t.Fatalf("old chunks left behind: %v", keys(fk.items))
	}
	if err := k.Delete("cred/big"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get("cred/big"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBrokerCachesUntilExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	calls := 0
	b := &Broker{
		Store: NewMemoryStore(),
		Now:   func() time.Time { return now },
		Fetch: func(context.Context, string) (provider.Credential, error) {
			calls++
			return provider.Token("tok", now.Add(15*time.Minute)), nil
		},
	}
	ctx := context.Background()
	for range 3 {
		ec, err := b.ExecCredential(ctx, "c1", false)
		if err != nil {
			t.Fatal(err)
		}
		if ec.Status.Token != "tok" || ec.Status.ExpirationTimestamp == nil {
			t.Fatalf("status: %+v", ec.Status)
		}
	}
	if calls != 1 {
		t.Fatalf("provider called %d times, want 1", calls)
	}

	now = now.Add(14*time.Minute + 30*time.Second) // within the skew of expiry
	if _, err := b.ExecCredential(ctx, "c1", false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expired credential not refreshed: %d calls", calls)
	}

	if _, err := b.ExecCredential(ctx, "c1", true); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("--refresh did not ask the provider: %d calls", calls)
	}
}

func TestBrokerStoredCredentialWithoutExpiry(t *testing.T) {
	b := &Broker{Store: NewMemoryStore()}
	if err := b.Put("c1", provider.ClientCert([]byte("CERT"), []byte("KEY"), provider.NoExpiry)); err != nil {
		t.Fatal(err)
	}
	ec, err := b.ExecCredential(context.Background(), "c1", false)
	if err != nil {
		t.Fatal(err)
	}
	if ec.Status.ClientCertificateData != "CERT" || ec.Status.ClientKeyData != "KEY" || ec.Status.ExpirationTimestamp != nil {
		t.Fatalf("status: %+v", ec.Status)
	}
	if ec.APIVersion != "client.authentication.k8s.io/v1" || ec.Kind != "ExecCredential" {
		t.Fatalf("type meta: %s %s", ec.APIVersion, ec.Kind)
	}
}

func TestBrokerDelegate(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	runs := 0
	b := &Broker{
		Store: NewMemoryStore(),
		Now:   func() time.Time { return now },
		RunExec: func(_ context.Context, cfg provider.ExecConfig) ([]byte, error) {
			runs++
			if cfg.Command != "aws" {
				t.Errorf("command = %q", cfg.Command)
			}
			return json.Marshal(map[string]any{
				"apiVersion": "client.authentication.k8s.io/v1beta1",
				"kind":       "ExecCredential",
				"status":     map[string]any{"token": "k8s-aws-v1.x", "expirationTimestamp": now.Add(10 * time.Minute)},
			})
		},
	}
	if err := b.Put("eks", provider.Delegate("aws", "eks", "get-token")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ec, err := b.ExecCredential(context.Background(), "eks", false)
		if err != nil {
			t.Fatal(err)
		}
		if ec.Status.Token != "k8s-aws-v1.x" {
			t.Fatalf("token = %q", ec.Status.Token)
		}
	}
	if runs != 1 {
		t.Fatalf("delegate ran %d times, want 1 (cached by expiry)", runs)
	}
}

func TestBrokerOIDCNotYetSupported(t *testing.T) {
	b := &Broker{Store: NewMemoryStore()}
	if err := b.Put("c", provider.OIDC("https://issuer", "tug")); err != nil {
		t.Fatal(err)
	}
	_, err := b.ExecCredential(context.Background(), "c", false)
	if provider.CodeOf(err) != provider.Unimplemented {
		t.Fatalf("want Unimplemented, got %v", err)
	}
}
