package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/paingha/tugcli/pkg/provider"
)

// call runs one protocol operation in process, as tug would run the binary,
// and returns stdout and the exit code.
func call(t *testing.T, op string, payload any) ([]byte, int) {
	t.Helper()
	req, err := provider.EncodeRequest(op, payload)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := provider.Run(context.Background(), example{}, []string{op}, bytes.NewReader(req), &out, &errb)
	return out.Bytes(), code
}

var demo = provider.Account{ID: "demo", Config: map[string]any{"clusters": "dev=https://127.0.0.1:6443, prod=https://10.0.0.1:6443"}}

var goodToken = map[string]string{"api_token": "ex_demo"}

func TestDescribe(t *testing.T) {
	out, code := call(t, provider.OpDescribe, nil)
	d, err := provider.DecodeDescribe(out)
	if err != nil || code != provider.ExitOK {
		t.Fatalf("describe = %+v, exit %d, %v", d, code, err)
	}
	if d.Name != "example" || d.Version != "0.1.0" {
		t.Fatalf("describe name, version = %q, %q; want example, 0.1.0", d.Name, d.Version)
	}
	if want := provider.CapList | provider.CapCredential | provider.CapServe; d.Capabilities != want {
		t.Fatalf("capabilities = %v, want %v", d.Capabilities, want)
	}
	if len(d.Secrets) != 1 || d.Secrets[0].Name != "api_token" || d.Secrets[0].Optional {
		t.Fatalf("secrets = %+v, want one required api_token", d.Secrets)
	}
}

func TestList(t *testing.T) {
	out, code := call(t, provider.OpList, provider.ListRequest{Account: demo, Secrets: goodToken})
	clusters, err := provider.DecodeList(out)
	if err != nil || code != provider.ExitOK {
		t.Fatalf("list = %+v, exit %d, %v", clusters, code, err)
	}
	want := []provider.Cluster{
		{ID: "dev", Name: "dev", Endpoints: provider.Endpoints{Public: "https://127.0.0.1:6443"}, Labels: map[string]string{"example": "true"}},
		{ID: "prod", Name: "prod", Endpoints: provider.Endpoints{Public: "https://10.0.0.1:6443"}, Labels: map[string]string{"example": "true"}},
	}
	if !reflect.DeepEqual(clusters, want) {
		t.Fatalf("list = %+v, want %+v", clusters, want)
	}
}

func TestListErrors(t *testing.T) {
	tests := []struct {
		name     string
		req      provider.ListRequest
		wantCode provider.Code
		wantHint string
	}{
		{"no token", provider.ListRequest{Account: demo}, provider.Unauthenticated, ""},
		{"bad token", provider.ListRequest{Account: demo, Secrets: map[string]string{"api_token": "nope"}}, provider.Unauthenticated, "start with ex_"},
		{"no clusters setting", provider.ListRequest{Account: provider.Account{ID: "demo"}, Secrets: goodToken}, provider.InvalidConfig, "--set clusters="},
		{
			"entry without a URL",
			provider.ListRequest{Account: provider.Account{ID: "demo", Config: map[string]any{"clusters": "dev"}}, Secrets: goodToken},
			provider.InvalidConfig, "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, code := call(t, provider.OpList, tt.req)
			_, err := provider.DecodeList(out)
			assertProviderError(t, err, tt.wantCode, tt.wantHint)
			if code != provider.ExitError {
				t.Fatalf("exit code = %d, want %d", code, provider.ExitError)
			}
		})
	}
}

func TestCredential(t *testing.T) {
	out, code := call(t, provider.OpCredential, provider.CredentialRequest{Account: demo, ClusterID: "dev", Secrets: goodToken})
	c, err := provider.DecodeCredential(out)
	if err != nil || code != provider.ExitOK {
		t.Fatalf("credential = %+v, exit %d, %v", c, code, err)
	}
	if c.Type != provider.CredentialToken || c.Token != "ex_demo" || c.ExpiresAt != nil {
		t.Fatalf("credential = %+v, want a non-expiring token ex_demo", c)
	}
}

func TestCredentialFromAuthCommand(t *testing.T) {
	acct := provider.Account{ID: "demo", Config: map[string]any{"clusters": demo.Config["clusters"], "authCommand": "example-auth"}}
	out, code := call(t, provider.OpCredential, provider.CredentialRequest{Account: acct, ClusterID: "prod", Secrets: goodToken})
	c, err := provider.DecodeCredential(out)
	if err != nil || code != provider.ExitOK {
		t.Fatalf("credential = %+v, exit %d, %v", c, code, err)
	}
	if c.Type != provider.CredentialDelegate || c.Exec.Command != "example-auth" || !reflect.DeepEqual(c.Exec.Args, []string{"token"}) {
		t.Fatalf("credential = %+v, want a delegate to example-auth token", c)
	}
	if !c.Exec.ProvideClusterInfo || string(c.Exec.ClusterConfig) != `{"cluster":"prod"}` {
		t.Fatalf("credential exec = %+v, want cluster information with the cluster config {\"cluster\":\"prod\"}", c.Exec)
	}
}

func TestBadTokenIsUnauthenticated(t *testing.T) {
	tests := []struct {
		name    string
		secrets map[string]string
		hint    string
	}{
		{"missing", nil, ""},
		{"wrong prefix", map[string]string{"api_token": "demo_x"}, "start with ex_"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _ := call(t, provider.OpCredential, provider.CredentialRequest{Account: demo, ClusterID: "dev", Secrets: tt.secrets})
			_, err := provider.DecodeCredential(out)
			assertProviderError(t, err, provider.Unauthenticated, tt.hint)
		})
	}
}

func TestUnsupportedOperations(t *testing.T) {
	for _, op := range []string{provider.OpAccounts, provider.OpRotate} {
		t.Run(op, func(t *testing.T) {
			var payload any
			if op == provider.OpRotate {
				payload = provider.CredentialRequest{Account: demo, ClusterID: "dev", Secrets: goodToken}
			}
			out, _ := call(t, op, payload)
			_, err := provider.DecodeCredential(out)
			assertProviderError(t, err, provider.Unimplemented, "")
		})
	}
}

// assertProviderError checks that err is a *provider.Error with code and, when
// hint is not empty, a hint containing it.
func assertProviderError(t *testing.T, err error, code provider.Code, hint string) {
	t.Helper()
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a *provider.Error with code %s", err, code)
	}
	if pe.Code != code {
		t.Fatalf("error code = %s (%v), want %s", pe.Code, err, code)
	}
	if hint != "" && !strings.Contains(pe.Hint, hint) {
		t.Fatalf("error hint = %q, want one containing %q", pe.Hint, hint)
	}
}
