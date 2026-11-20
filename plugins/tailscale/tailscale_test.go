package tailscale

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

// statusJSON is tailscale status --json on a laptop in a tailnet with two
// operators, an ingress proxy, a phone and a dedicated API server proxy
// with its own tag.
const statusJSON = `{
  "Version": "1.90.6-t0123456789-gabcdef01234",
  "TUN": true,
  "BackendState": "Running",
  "HaveNodeKey": true,
  "AuthURL": "",
  "TailscaleIPs": ["100.101.102.103", "fd7a:115c:a1e0::1"],
  "Self": {
    "ID": "nSelf1CNTRL",
    "PublicKey": "nodekey:0000",
    "HostName": "laptop",
    "DNSName": "laptop.tail1234.ts.net.",
    "OS": "macOS",
    "UserID": 123,
    "TailscaleIPs": ["100.101.102.103"],
    "Tags": ["tag:k8s-operator"],
    "Online": true
  },
  "Health": [],
  "MagicDNSSuffix": "tail1234.ts.net",
  "CurrentTailnet": {"Name": "example.com", "MagicDNSSuffix": "tail1234.ts.net", "MagicDNSEnabled": true},
  "CertDomains": ["laptop.tail1234.ts.net"],
  "Peer": {
    "nodekey:1111": {
      "ID": "nProd1CNTRL", "PublicKey": "nodekey:1111", "HostName": "tailscale-operator",
      "DNSName": "prod-operator.tail1234.ts.net.", "OS": "linux", "UserID": 456,
      "TailscaleIPs": ["100.64.0.1"], "Tags": ["tag:k8s-operator"], "Online": true, "Active": false
    },
    "nodekey:2222": {
      "ID": "nDev2CNTRL", "PublicKey": "nodekey:2222", "HostName": "tailscale-operator",
      "DNSName": "dev-operator.tail1234.ts.net.", "OS": "linux", "UserID": 456,
      "TailscaleIPs": ["100.64.0.2"], "Tags": ["tag:dev", "tag:k8s-operator"], "Online": false
    },
    "nodekey:3333": {
      "ID": "nWeb3CNTRL", "PublicKey": "nodekey:3333", "HostName": "web",
      "DNSName": "web.tail1234.ts.net.", "OS": "linux", "UserID": 456,
      "TailscaleIPs": ["100.64.0.3"], "Tags": ["tag:k8s"], "Online": true
    },
    "nodekey:4444": {
      "ID": "nPhone4CNTRL", "PublicKey": "nodekey:4444", "HostName": "phone",
      "DNSName": "phone.tail1234.ts.net.", "OS": "iOS", "UserID": 123,
      "TailscaleIPs": ["100.64.0.4"], "Online": true
    },
    "nodekey:5555": {
      "ID": "nStaging5CNTRL", "PublicKey": "nodekey:5555", "HostName": "staging-api",
      "DNSName": "staging-api.tail1234.ts.net.", "OS": "linux", "UserID": 456,
      "TailscaleIPs": ["100.64.0.5"], "Tags": ["tag:k8s-api"], "Online": true
    }
  },
  "User": {"123": {"ID": 123, "LoginName": "alice@example.com", "DisplayName": "Alice"}},
  "ClientVersion": null
}`

// fakeTailscale puts a shell script called tailscale first on PATH. It
// prints status for tailscale status --json and fails on anything else.
func fakeTailscale(t *testing.T, status string) {
	t.Helper()
	fakeTool(t, "#!/bin/sh\n"+
		"[ \"$*\" = \"status --json\" ] || { echo \"unexpected arguments: $*\" >&2; exit 2; }\n"+
		"cat <<'EOF'\n"+status+"\nEOF\n")
}

func fakeTool(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake tailscale is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestList(t *testing.T) {
	fakeTailscale(t, statusJSON)
	tests := []struct {
		name      string
		config    map[string]any
		ids       []string
		endpoints []string
	}{
		{
			name:      "operator tag",
			ids:       []string{"nDev2CNTRL", "nProd1CNTRL"},
			endpoints: []string{"https://dev-operator.tail1234.ts.net", "https://prod-operator.tail1234.ts.net"},
		},
		{
			name:      "account tag",
			config:    map[string]any{ConfigTag: "tag:k8s-api"},
			ids:       []string{"nStaging5CNTRL"},
			endpoints: []string{"https://staging-api.tail1234.ts.net"},
		},
		{
			name:   "nothing tagged",
			config: map[string]any{ConfigTag: "tag:nothing"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusters, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "example.com", Config: tt.config}})
			if err != nil {
				t.Fatal(err)
			}
			var ids, endpoints []string
			for _, c := range clusters {
				ids = append(ids, c.ID)
				endpoints = append(endpoints, c.Endpoints.Private)
				if c.Endpoints.Public != "" || c.CAData != nil || c.TLSServerName != "" {
					t.Errorf("%s: endpoints %+v, CA %q, server name %q, want only a private endpoint", c.ID, c.Endpoints, c.CAData, c.TLSServerName)
				}
				// What tailscale configure kubeconfig writes for the user.
				if cr := c.Credential; cr == nil || cr.Type != provider.CredentialToken || cr.Token != "unused" || cr.ExpiresAt != nil {
					t.Errorf("%s: credential = %+v, want the token \"unused\" without expiry", c.ID, cr)
				}
			}
			if !slices.Equal(ids, tt.ids) || !slices.Equal(endpoints, tt.endpoints) {
				t.Errorf("List = %v at %v, want %v at %v", ids, endpoints, tt.ids, tt.endpoints)
			}
		})
	}
}

func TestListNames(t *testing.T) {
	fakeTailscale(t, statusJSON)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range clusters {
		names = append(names, c.Name)
	}
	// Both operators have the host name tailscale-operator; their MagicDNS
	// names tell them apart.
	if want := []string{"dev-operator", "prod-operator"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestListStates(t *testing.T) {
	tests := []struct {
		state string
		code  provider.Code
		hint  string
	}{
		{"NeedsLogin", provider.Unauthenticated, "run `tailscale login`"},
		{"NeedsMachineAuth", provider.Unauthenticated, "ask an admin to approve the device on the Machines page of the Tailscale admin console"},
		{"Stopped", provider.Unavailable, "run `tailscale up`"},
		{"Starting", provider.Unavailable, "run `tailscale up`"},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			fakeTailscale(t, strings.Replace(statusJSON, `"BackendState": "Running"`, `"BackendState": "`+tt.state+`"`, 1))
			_, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "example.com"}})
			if pe := provider.AsError(err); pe.Code != tt.code || pe.Hint != tt.hint {
				t.Errorf("List = %+v, want %s with hint %q", pe, tt.code, tt.hint)
			}
		})
	}
}

func TestListWithoutTailscaled(t *testing.T) {
	const message = "failed to connect to local tailscaled; it doesn't appear to be running (sudo systemctl start tailscaled ?)"
	fakeTool(t, "#!/bin/sh\ncat >&2 <<'EOF'\n"+message+"\nEOF\nexit 1\n")
	_, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "example.com"}})
	pe := provider.AsError(err)
	if pe.Code != provider.Unavailable || pe.Message != "tailscale status: "+message || pe.Hint == "" {
		t.Errorf("List = %+v, want Unavailable with tailscale's message and a hint", pe)
	}
}

func TestListWithoutTailscale(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "example.com"}})
	if pe := provider.AsError(err); pe.Code != provider.Unavailable || pe.Hint != InstallHint {
		t.Errorf("List = %+v, want Unavailable with the install hint", pe)
	}
}

func TestCredential(t *testing.T) {
	c, err := New().Credential(context.Background(), provider.CredentialRequest{Account: provider.Account{ID: "example.com"}, ClusterID: "nProd1CNTRL"})
	if err != nil || c.Type != provider.CredentialToken || c.Token != "unused" || c.ExpiresAt != nil {
		t.Errorf("Credential = %+v, %v, want the token \"unused\" without expiry", c, err)
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	bin := filepath.Join(t.TempDir(), "tug-provider-tailscale")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-tailscale").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	fakeTailscale(t, statusJSON)
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:    Name,
		Account: provider.Account{ID: "example.com"},
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the tailscale plugin does not conform")
	}
}
