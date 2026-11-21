package plugin

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// indexKey is a signing key as NewIndexKey makes one, and its public half.
type indexKey struct {
	private string
	public  ed25519.PublicKey
}

func newTestIndexKey(t *testing.T) indexKey {
	t.Helper()
	priv, pub, err := NewIndexKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParsePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return indexKey{priv, key}
}

// writeTestIndex writes indexYAML with real sha256s to a file.
func writeTestIndex(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.yaml")
	body := strings.Replace(strings.Replace(indexYAML, "%s", strings.Repeat("a", 64), 1), "%s", strings.Repeat("b", 64), 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// signFile signs the index file at path with key and returns its bytes and
// signature.
func signFile(t *testing.T, path string, key indexKey) (data, sig []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sig, err = SignIndex(data, key.private)
	if err != nil {
		t.Fatal(err)
	}
	return data, sig
}

func TestIndexKeyRotation(t *testing.T) {
	k1, k2, k3 := newTestIndexKey(t), newTestIndexKey(t), newTestIndexKey(t)
	path := writeTestIndex(t)

	data, sig := signFile(t, path, k1)
	ix, err := ParseIndex(data, sig, k1.public)
	if err != nil {
		t.Fatal(err)
	}
	if !ix.SignedBy().Equal(k1.public) {
		t.Fatal("SignedBy() is not the key the index was signed with")
	}

	// k1 hands over to k2, and the index is signed with k2: someone who
	// trusts k1 reaches it.
	if _, _, err := RotateIndexKey(path, k1.private, k2.private); err != nil {
		t.Fatal(err)
	}
	if _, err := SignIndex(mustRead(t, path), k1.private); err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("SignIndex with the superseded key = %v, want a refusal", err)
	}
	data, sig = signFile(t, path, k2)
	if ix, err = ParseIndex(data, sig, k1.public); err != nil {
		t.Fatalf("ParseIndex trusting the old key after a handover: %v", err)
	}
	if !ix.SignedBy().Equal(k2.public) {
		t.Fatal("SignedBy() is not the successor")
	}

	// And on to k3, through both handovers.
	if _, _, err := RotateIndexKey(path, k2.private, k3.private); err != nil {
		t.Fatal(err)
	}
	data, sig = signFile(t, path, k3)
	for _, start := range []indexKey{k1, k2, k3} {
		if ix, err := ParseIndex(data, sig, start.public); err != nil || !ix.SignedBy().Equal(k3.public) {
			t.Fatalf("ParseIndex trusting an earlier key = %v, want it signed by the newest", err)
		}
	}

	// An index signed by a superseded key is refused, whoever made it.
	forgedSig := []byte(base64Std(ed25519.Sign(mustDecode(t, k2.private), data)))
	if _, err := ParseIndex(data, forgedSig, k2.public); err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("ParseIndex of an index signed by a superseded key = %v, want a refusal", err)
	}

	// A key that did not hand over reaches nothing.
	other := newTestIndexKey(t)
	if _, err := ParseIndex(data, sig, other.public); err == nil || !strings.Contains(err.Error(), "does not match its key") {
		t.Fatalf("ParseIndex with an unrelated key = %v, want a refusal", err)
	}
}

func TestIndexSupersededKeysValidate(t *testing.T) {
	k1, k2 := newTestIndexKey(t), newTestIndexKey(t)
	path := writeTestIndex(t)
	if _, _, err := RotateIndexKey(path, k1.private, k2.private); err != nil {
		t.Fatal(err)
	}
	good := string(mustRead(t, path))
	k1b64, k2b64 := base64Std(k1.public), base64Std(k2.public)
	tests := []struct{ name, from, to, want string }{
		{"forged handover", "successor: " + k2b64, "successor: " + base64Std(newTestIndexKey(t).public), "not that key's over its successor"},
		{"successor without signature", "  signature:", "  oldSignature:", "unknown field"},
		{"bad key", "publicKey: " + k1b64, "publicKey: nope", "base64 ed25519 key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Replace(good, tt.from, tt.to, 1)
			if body == good {
				t.Fatalf("%q is not in the index:\n%s", tt.from, good)
			}
			if _, err := SignIndex([]byte(body), k2.private); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("SignIndex = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
	retired := strings.Replace(good, "  successor: "+k2b64+"\n", "", 1)
	if _, err := SignIndex([]byte(retired), k2.private); err == nil || !strings.Contains(err.Error(), "both a successor") {
		t.Errorf("SignIndex of a handover without a successor = %v, want a refusal", err)
	}
}

func TestIndexRevocations(t *testing.T) {
	key := newTestIndexKey(t)
	path := writeTestIndex(t)
	r, err := RevokeInIndex(path, Revocation{SHA256: strings.Repeat("A", 64), Reason: "leaks tokens"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Plugin != "acme" || r.Version != "0.9.0" || r.SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("RevokeInIndex = %+v, want acme 0.9.0 from the index's entry", r)
	}
	if got := r.String(); got != "acme 0.9.0 (leaks tokens)" {
		t.Errorf("String() = %q", got)
	}
	// Revoking it again replaces the entry.
	if _, err := RevokeInIndex(path, Revocation{SHA256: strings.Repeat("a", 64), Reason: "leaks tokens to a third party"}); err != nil {
		t.Fatal(err)
	}
	data, sig := signFile(t, path, key)
	ix, err := ParseIndex(data, sig, key.public)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Revoked) != 1 {
		t.Fatalf("Revoked = %+v, want one entry", ix.Revoked)
	}
	loaded := []Loaded{{Source: IndexSource{Name: "main"}, Index: ix}}
	if r, by, ok := FindRevoked(loaded, strings.Repeat("A", 64)); !ok || by != "main" || r.Reason != "leaks tokens to a third party" {
		t.Errorf("FindRevoked = %+v, %q, %v", r, by, ok)
	}
	if _, _, ok := FindRevoked(loaded, strings.Repeat("b", 64)); ok {
		t.Error("FindRevoked found an archive that is not revoked")
	}
	if _, _, ok := FindRevokedVersion(loaded, "acme", "v0.9.0"); !ok {
		t.Error("FindRevokedVersion(acme, v0.9.0) found nothing")
	}
	if v, _ := ix.Plugins[0].LatestFunc(func(v IndexVersion) bool { return v.Version != "0.10.0" }); v.Version != "0.9.0" {
		t.Errorf("LatestFunc skipping 0.10.0 = %s, want 0.9.0", v.Version)
	}

	for _, bad := range []Revocation{{SHA256: "abc"}, {SHA256: strings.Repeat("z", 64)}, {SHA256: strings.Repeat("a", 64), Plugin: "Bad_Name"}, {SHA256: strings.Repeat("a", 64), Plugin: "acme", Version: "../1"}} {
		if _, err := RevokeInIndex(path, bad); err == nil {
			t.Errorf("RevokeInIndex(%+v) succeeded, want a refusal", bad)
		}
	}
}

// TestIndexCacheRecordsKeyChange checks that once an index has handed over
// to a new key, tug refuses it signed with the old one, as it would be by
// someone who has the old key.
func TestIndexCacheRecordsKeyChange(t *testing.T) {
	k1, k2 := newTestIndexKey(t), newTestIndexKey(t)
	path := writeTestIndex(t)
	before := mustRead(t, path)
	if _, _, err := RotateIndexKey(path, k1.private, k2.private); err != nil {
		t.Fatal(err)
	}
	rotated, rotatedSig := signFile(t, path, k2)
	oldSig, err := SignIndex(before, k1.private)
	if err != nil {
		t.Fatal(err)
	}

	serve := struct{ data, sig []byte }{rotated, rotatedSig}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, SignatureSuffix) {
			_, _ = w.Write(serve.sig)
			return
		}
		_, _ = w.Write(serve.data)
	}))
	defer srv.Close()

	now := time.Now()
	c := &IndexCache{Dir: t.TempDir(), KeysFile: filepath.Join(t.TempDir(), "keys.yaml"), Now: func() time.Time { return now }}
	src := IndexSource{Name: "main", URL: srv.URL + "/index.yaml", PublicKey: k1.public}
	loaded, errs := c.LoadAll(context.Background(), []IndexSource{src})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if key, changed := loaded[0].KeyChanged(); !changed || key != base64Std(k2.public) {
		t.Fatalf("KeyChanged() = %q, %v, want the new key", key, changed)
	}
	if _, err := os.Stat(c.KeysFile); err != nil {
		t.Fatalf("the key change was not recorded: %v", err)
	}

	// The old index, signed with the old key, is refused from now on.
	serve.data, serve.sig = before, oldSig
	now = now.Add(2 * indexTTL)
	if _, err := c.Load(context.Background(), src); err == nil || !strings.Contains(err.Error(), "does not match its key") {
		t.Fatalf("Load of an index signed with the replaced key = %v, want a refusal", err)
	}
	// Without the record it would have been accepted.
	fresh := &IndexCache{Dir: t.TempDir(), Now: c.Now}
	if _, err := fresh.Load(context.Background(), src); err != nil {
		t.Fatalf("Load without a record: %v", err)
	}
	// Once config.yaml names the new key, the record no longer matters.
	src.PublicKey = k2.public
	serve.data, serve.sig = rotated, rotatedSig
	now = now.Add(2 * indexTTL)
	if l, errs := c.LoadAll(context.Background(), []IndexSource{src}); len(errs) > 0 {
		t.Fatal(errs)
	} else if _, changed := l[0].KeyChanged(); changed {
		t.Fatal("KeyChanged() after config.yaml names the new key")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustDecode(t *testing.T, s string) ed25519.PrivateKey {
	t.Helper()
	k, err := parsePrivateKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
