package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

type fake struct{}

func (fake) Info() Info {
	return Info{Name: "fake", Version: "1.2.3", Secrets: []SecretSpec{{Name: "api_token"}}}
}

func (fake) List(_ context.Context, req ListRequest) ([]Cluster, error) {
	if req.Secrets["api_token"] != "good" {
		return nil, Errorf(Unauthenticated, "bad token")
	}
	return []Cluster{{ID: "c-1", Name: "one", Endpoints: Endpoints{Public: "https://one"}, CAData: []byte("-----BEGIN")}}, nil
}

func (fake) Credential(_ context.Context, req CredentialRequest) (Credential, error) {
	if req.ClusterID == "bad" {
		return Credential{Type: CredentialToken}, nil // invalid: no token
	}
	return Token("tok-"+req.ClusterID, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)), nil
}

func run(t *testing.T, op, in string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), fake{}, []string{op}, strings.NewReader(in), &out, &errb)
	return code, out.String()
}

func TestDescribe(t *testing.T) {
	code, out := run(t, OpDescribe, "")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	d, err := DecodeDescribe([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "fake" || !d.Capabilities.Has(CapList|CapCredential) || d.Capabilities.Has(CapAccounts) {
		t.Fatalf("unexpected describe: %+v", d)
	}
	if len(d.Protocols) == 0 || d.Protocols[0] != APIVersion {
		t.Fatalf("protocols: %v", d.Protocols)
	}
	if !strings.Contains(out, `"capabilities":["list","credential"]`) {
		t.Fatalf("capabilities not encoded as names: %s", out)
	}
}

func TestListRoundTrip(t *testing.T) {
	req := `{"apiVersion":"provider/v1alpha1","kind":"ListRequest","account":{"id":"work","config":{"org":"acme"}},"secrets":{"api_token":"good"}}`
	code, out := run(t, OpList, req)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	clusters, err := DecodeList([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || string(clusters[0].CAData) != "-----BEGIN" {
		t.Fatalf("clusters: %+v", clusters)
	}
	if !strings.Contains(out, `"caData":"LS0tLS1CRUdJTg=="`) {
		t.Fatalf("caData should be base64 on the wire: %s", out)
	}
}

func TestErrorsAreJSONWithNonZeroExit(t *testing.T) {
	code, out := run(t, OpList, `{"kind":"ListRequest","secrets":{"api_token":"nope"}}`)
	if code != ExitError {
		t.Fatalf("exit %d", code)
	}
	_, err := DecodeList([]byte(out))
	if CodeOf(err) != Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestUnimplementedAndUnknown(t *testing.T) {
	if code, out := run(t, OpAccounts, ""); code != ExitError || !strings.Contains(out, string(Unimplemented)) {
		t.Fatalf("accounts: %d %s", code, out)
	}
	if code, _ := run(t, "bogus", ""); code != ExitUsage {
		t.Fatalf("unknown op exit %d", code)
	}
}

func TestCredentialValidated(t *testing.T) {
	code, out := run(t, OpCredential, `{"kind":"CredentialRequest","clusterID":"c-1"}`)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	c, err := DecodeCredential([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "tok-c-1" || c.ExpiresAt == nil {
		t.Fatalf("credential: %+v", c)
	}
	code, out = run(t, OpCredential, `{"kind":"CredentialRequest","clusterID":"bad"}`)
	if code != ExitError || !strings.Contains(out, "invalid credential") {
		t.Fatalf("invalid credential should fail: %d %s", code, out)
	}
}

func TestWrongKindRejected(t *testing.T) {
	code, out := run(t, OpList, `{"kind":"CredentialRequest"}`)
	if code != ExitError || !strings.Contains(out, string(InvalidConfig)) {
		t.Fatalf("%d %s", code, out)
	}
}

func TestEncodeRequestRoundTrip(t *testing.T) {
	b, err := EncodeRequest(OpList, ListRequest{Account: Account{ID: "work"}, Secrets: map[string]string{"api_token": "good"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kind":"ListRequest"`) || !strings.Contains(string(b), `"apiVersion":"provider/v1alpha1"`) {
		t.Fatalf("envelope missing: %s", b)
	}
	code, out := run(t, OpList, string(b))
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := EncodeRequest(OpList, CredentialRequest{}); err == nil {
		t.Fatal("wrong payload type should fail")
	}
}

func TestCapabilityJSONIgnoresUnknown(t *testing.T) {
	var c Capability
	if err := json.Unmarshal([]byte(`["list","teleport","rotate"]`), &c); err != nil {
		t.Fatal(err)
	}
	if c != CapList|CapRotate {
		t.Fatalf("got %v", c)
	}
}

func TestCancelWhileReadingRequest(t *testing.T) {
	r, w := io.Pipe() // never written: the host is still sending
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	var out, errb bytes.Buffer
	start := time.Now()
	code := Run(ctx, fake{}, []string{OpList}, r, &out, &errb)
	if code != ExitError || time.Since(start) > time.Second {
		t.Fatalf("exit %d after %s", code, time.Since(start))
	}
	if _, err := DecodeList(out.Bytes()); CodeOf(err) != Unavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
}
