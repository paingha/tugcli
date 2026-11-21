package python

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// wirePlugin is the Go copy of Wire in testdata/wire.py: change both
// together. Its fixtures cover every field of every message, and strings that
// Go escapes.
type wirePlugin struct{}

var (
	_ provider.Provider      = wirePlugin{}
	_ provider.AccountLister = wirePlugin{}
	_ provider.Rotator       = wirePlugin{}
	_ provider.AccessChecker = wirePlugin{}
)

var (
	wireExpiry = time.Date(2030, 1, 2, 3, 4, 5, 123456000, time.UTC)
	wireCA     = []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
)

func fullCluster() provider.Cluster {
	inline := provider.Token("inline", wireExpiry)
	return provider.Cluster{
		ID: "c-1", Name: "prod-eu", Region: "eu-west", Version: "1.33",
		Endpoints:             provider.Endpoints{Public: "https://a.example:443", Private: "https://10.0.0.1:6443"},
		CAData:                wireCA,
		TLSServerName:         "kubernetes",
		InsecureSkipTLSVerify: true,
		ProxyURL:              "socks5://127.0.0.1:1080",
		DefaultNamespace:      "payments",
		Tunnel:                "ssh://admin@bastion.example.com",
		Labels:                map[string]string{"team": "platform", "env": "prod", "gpu": "true"},
		Credential:            &inline,
	}
}

func (wirePlugin) Info() provider.Info {
	return provider.Info{
		Name: "wire", Version: "1.0.0", Description: "Wire <format> & fixtures",
		Secrets: []provider.SecretSpec{{Name: "api_token", Prompt: "API token"}, {Name: "region_key", Optional: true}},
	}
}

func (wirePlugin) Accounts(context.Context) ([]provider.Account, error) {
	return []provider.Account{
		{ID: "work", Description: "café profile", Config: map[string]any{
			"profile": "work", "nested": map[string]any{"b": 1, "a": []any{true, nil, "x"}}, "count": 3,
		}},
		{ID: "home"},
	}, nil
}

func (wirePlugin) List(_ context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	switch req.Secrets["api_token"] {
	case "good":
	case "empty":
		return nil, nil
	case "limited":
		return nil, &provider.Error{
			Code: provider.RateLimited, Message: "GET /v1/clusters: slow <down> & wait",
			Hint: "wait \u2028 a minute", RetryAfterSeconds: 30,
		}
	default:
		return nil, provider.Errorf(provider.Unauthenticated, "no api_token").WithHint("set one")
	}
	return []provider.Cluster{
		fullCluster(),
		{ID: "c-2", Name: "dev", Endpoints: provider.Endpoints{Public: "https://b.example"}},
		{
			ID: "c-3", Name: "ünïcode-東京", Endpoints: provider.Endpoints{Private: "https://c.example"},
			Labels: map[string]string{"note": "a<b>&c\u2028\u2029\x01\t\"\\", "account": req.Account.ID},
		},
	}, nil
}

func (wirePlugin) Credential(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	switch id := req.ClusterID; id {
	case "token":
		return provider.Token("tok-"+req.Account.ID, wireExpiry), nil
	case "token-local":
		return provider.Token("tok", time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("CET", 3600))), nil
	case "noexpiry":
		return provider.Token("tok", provider.NoExpiry), nil
	case "cert":
		return provider.ClientCert(wireCA, []byte("KEY"), wireExpiry), nil
	case "oidc":
		c := provider.OIDC("https://issuer.example", "tug", "openid", "offline_access")
		c.ClientSecret = "s3cret"
		return c, nil
	case "delegate":
		c := provider.Delegate("aws", "eks", "get-token", "--cluster-name", id)
		c.Exec.Env = map[string]string{"B": "2", "A": "1"}
		c.Exec.APIVersion = "client.authentication.k8s.io/v1"
		c.Exec.InteractiveMode = "IfAvailable"
		c.Exec.InstallHint = "install <aws> & co"
		return c, nil
	case "invalid":
		return provider.Credential{Type: provider.CredentialToken}, nil
	default:
		return provider.Credential{}, provider.Errorf(provider.NotFound, "no cluster %q", id)
	}
}

func (wirePlugin) Rotate(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	return provider.Token(fmt.Sprintf("rotated-%s-%s", req.ClusterID, req.Secrets["api_token"]), wireExpiry), nil
}

func (wirePlugin) CheckAccess(_ context.Context, req provider.AccessRequest) ([]provider.Finding, error) {
	c := req.Cluster
	findings := []provider.Finding{
		{Severity: provider.SeverityError, Message: "allow list lacks " + req.EgressIP, Fix: "add " + req.EgressIP + "/32"},
		{Severity: provider.SeverityInfo, Message: fmt.Sprintf("%s in %s has %d bytes of CA at %s", c.Name, req.Account.ID, len(c.CAData), c.Endpoints.Public)},
	}
	if c.Credential != nil {
		expires := "never"
		if c.Credential.ExpiresAt != nil {
			expires = c.Credential.ExpiresAt.Format(time.RFC3339Nano)
		}
		findings = append(findings, provider.Finding{
			Severity: provider.SeverityWarning,
			Message:  fmt.Sprintf("inline %s credential expires %s", c.Credential.Type, expires),
		})
	}
	return findings, nil
}

// wirePayloads are the requests `wire.py encode` encodes, in its order.
func wirePayloads() []struct {
	op      string
	payload any
} {
	secrets := map[string]string{"z": "1", "a": "<&>"}
	return []struct {
		op      string
		payload any
	}{
		{provider.OpDescribe, nil},
		{provider.OpAccounts, nil},
		{provider.OpList, provider.ListRequest{
			Account: provider.Account{ID: "work", Description: "d", Config: map[string]any{"b": "2", "a": map[string]any{"z": []any{1, "x"}}}},
			Secrets: secrets,
		}},
		{provider.OpList, provider.ListRequest{}},
		{provider.OpCredential, provider.CredentialRequest{Account: provider.Account{ID: "demo"}, ClusterID: "dev", Secrets: secrets}},
		{provider.OpRotate, provider.CredentialRequest{Account: provider.Account{ID: "demo"}, ClusterID: "dev"}},
		{provider.OpCheckAccess, provider.AccessRequest{Account: provider.Account{ID: "w"}, Cluster: fullCluster(), EgressIP: "198.51.100.9", Secrets: secrets}},
		{provider.OpCheckAccess, provider.AccessRequest{}},
	}
}

func sortedLines(s string) []string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if s == "" {
		lines = nil
	}
	slices.Sort(lines)
	return lines
}

// lineDiff lists the lines only one of two sorted lists has.
func lineDiff(want, got []string) string {
	count := map[string]int{}
	for _, l := range want {
		count[l]++
	}
	for _, l := range got {
		count[l]--
	}
	var b strings.Builder
	for _, l := range want {
		if count[l] > 0 {
			fmt.Fprintf(&b, "missing: %s\n", l)
			count[l]--
		}
	}
	for _, l := range got {
		if count[l] < 0 {
			fmt.Fprintf(&b, "extra:   %s\n", l)
			count[l]++
		}
	}
	return b.String()
}

func TestWireFormatMatchesGoSDK(t *testing.T) {
	python := needPython(t)
	script := filepath.Join("testdata", "wire.py")
	env := pythonEnv(t)

	listReq := `{"apiVersion":"provider/v1alpha1","kind":"ListRequest","account":{"id":"work","config":{"clusters":"x"}},"secrets":{"api_token":"good"}}`
	credReq := func(id string) string {
		return `{"apiVersion":"provider/v1alpha1","kind":"CredentialRequest","account":{"id":"demo"},"clusterID":"` + id + `"}`
	}
	accessReq := `{"apiVersion":"provider/v1alpha1","kind":"CheckAccessRequest","account":{"id":"work"},` +
		`"cluster":{"id":"101","name":"prod-eu","endpoints":{"public":"https://101.example.com:443"},"caData":"Q0E=",` +
		`"credential":{"type":"token","token":"t","expiresAt":"2030-01-02T03:04:05.5+01:00"}},"egressIP":"198.51.100.9","secrets":{"api_token":"x"}}`
	tests := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"describe", []string{"describe"}, ""},
		{"accounts", []string{"accounts"}, ""},
		{"accounts with a request", []string{"accounts"}, `{"apiVersion":"provider/v1alpha1","kind":"AccountsRequest"}`},
		{"list", []string{"list"}, listReq},
		{"list without an envelope", []string{"list"}, `{"secrets":{"api_token":"good"}}`},
		{"list of nothing", []string{"list"}, `{"kind":"ListRequest","secrets":{"api_token":"empty"}}`},
		{"list rate limited", []string{"list"}, `{"kind":"ListRequest","secrets":{"api_token":"limited"}}`},
		{"list unauthenticated", []string{"list"}, `{"kind":"ListRequest","secrets":{"api_token":"bad"}}`},
		{"list of null", []string{"list"}, `null`},
		{"list with an empty request", []string{"list"}, ""},
		{"list with the wrong kind", []string{"list"}, `{"kind":"CredentialRequest"}`},
		{"list in another version", []string{"list"}, `{"apiVersion":"provider/v9","kind":"ListRequest"}`},
		{"token", []string{"credential"}, credReq("token")},
		{"token in local time", []string{"credential"}, credReq("token-local")},
		{"token without expiry", []string{"credential"}, credReq("noexpiry")},
		{"client cert", []string{"credential"}, credReq("cert")},
		{"oidc", []string{"credential"}, credReq("oidc")},
		{"delegate", []string{"credential"}, credReq("delegate")},
		{"invalid credential", []string{"credential"}, credReq("invalid")},
		{"unknown cluster", []string{"credential"}, credReq("missing")},
		{"rotate", []string{"rotate"}, `{"kind":"RotateRequest","clusterID":"dev","secrets":{"api_token":"old"}}`},
		{"rotate with the credential kind", []string{"rotate"}, `{"kind":"CredentialRequest","clusterID":"dev"}`},
		{"checkAccess", []string{"checkAccess"}, accessReq},
		{"checkAccess with an empty request", []string{"checkAccess"}, ""},
		{"serve", []string{"serve"}, ""},
		{"unknown operation", []string{"bogus"}, ""},
		{"no operation", nil, ""},
		{"two operations", []string{"list", "list"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var want, goStderr bytes.Buffer
			wantCode := provider.Run(context.Background(), wirePlugin{}, tt.args, strings.NewReader(tt.stdin), &want, &goStderr)

			cmd := exec.Command(python, append([]string{script}, tt.args...)...)
			cmd.Env = env
			cmd.Stdin = strings.NewReader(tt.stdin)
			var got, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &got, &stderr
			gotCode := exitCode(cmd.Run())
			if gotCode != wantCode || got.String() != want.String() {
				t.Errorf("wire.py %v exited %d and wrote\n%s\nwant exit %d and\n%s\nstderr:\n%s",
					tt.args, gotCode, got.String(), wantCode, want.String(), stderr.String())
			}
		})
	}

	t.Run("serve session", func(t *testing.T) {
		// Every one-shot case with one operation becomes a frame, followed by
		// frames that only serve mode has: cancels, and lines it cannot read.
		var frames []string
		for _, tt := range tests {
			if len(tt.args) != 1 {
				continue
			}
			id, err := json.Marshal(tt.name)
			if err != nil {
				t.Fatal(err)
			}
			frame := `{"id":` + string(id) + `,"op":"` + tt.args[0] + `"`
			if tt.stdin != "" {
				frame += `,"request":` + tt.stdin
			}
			frames = append(frames, frame+"}")
		}
		frames = append(frames,
			`{"id":"no op"}`,
			`{"id":"null request","op":"credential","request":null}`,
			`{"id":"a cancel for no request","cancel":true}`,
			``,
			`   `,
			`this is not JSON`,
			`{"op":"describe"}`,
			`{"id":"","op":"describe"}`,
			`{"id":7,"op":"describe"}`,
			`{"id":"bad cancel","op":"describe","cancel":"yes"}`,
			`{"id":"bad op","op":1}`,
			`[1]`,
			`null`,
		)
		stdin := strings.Join(frames, "\n") + "\n"

		var want bytes.Buffer
		// Requests run in goroutines that log concurrently, so the Go side's
		// stderr is discarded rather than shared.
		wantCode := provider.Run(context.Background(), wirePlugin{}, []string{provider.OpServe}, strings.NewReader(stdin), &want, io.Discard)

		cmd := exec.Command(python, script, provider.OpServe)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(stdin)
		var got, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &got, &stderr
		gotCode := exitCode(cmd.Run())

		// Answers may come in any order, so compare them as sorted lines;
		// each line holds its request's id.
		wantLines, gotLines := sortedLines(want.String()), sortedLines(got.String())
		if gotCode != wantCode || !slices.Equal(gotLines, wantLines) {
			t.Errorf("wire.py serve exited %d, want %d\n%s\nstderr:\n%s", gotCode, wantCode, lineDiff(wantLines, gotLines), stderr.String())
		}
		if len(wantLines) < len(frames)-5 {
			t.Errorf("the Go SDK answered only %d of %d frames:\n%s", len(wantLines), len(frames), want.String())
		}
	})

	t.Run("encode_request", func(t *testing.T) {
		cmd := exec.Command(python, script, "encode")
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("wire.py encode: %v\n%s", err, stderr.String())
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		payloads := wirePayloads()
		if len(lines) != len(payloads) {
			t.Fatalf("wire.py encode wrote %d requests, want %d:\n%s", len(lines), len(payloads), out)
		}
		for i, p := range payloads {
			want, err := provider.EncodeRequest(p.op, p.payload)
			if err != nil {
				t.Fatal(err)
			}
			if lines[i] != string(want) {
				t.Errorf("encode_request(%s) [%d] =\n%s\nwant\n%s", p.op, i, lines[i], want)
			}
		}
	})
}
