// Command tug-provider-example is a minimal tug provider plugin built with the
// Go SDK. It lists the clusters named in its account config and hands out the
// account's api_token as a bearer token, or, with the authCommand setting,
// has tug run a kubectl exec plugin for the credential instead.
//
// Try it from a shell:
//
//	go build -o bin/tug-provider-example .
//	echo '{"kind":"ListRequest","account":{"id":"demo","config":{"clusters":"dev=https://127.0.0.1:6443"}},"secrets":{"api_token":"ex_demo"}}' \
//	  | ./bin/tug-provider-example list
//
// Check it against the protocol with `tug plugin test`.
package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/paingha/tugcli/pkg/provider"
)

// tokenPrefix marks this provider's tokens. Checking it lets the plugin answer
// Unauthenticated for an obviously wrong token without calling an API, as a
// real provider would after its API returned 401.
const tokenPrefix = "ex_"

type example struct{}

func (example) Info() provider.Info {
	return provider.Info{
		Name:        "example",
		Version:     "0.1.0",
		Description: "Example clusters, for learning the plugin protocol",
		Secrets:     []provider.SecretSpec{{Name: "api_token", Prompt: "Example API token (starts with ex_)"}},
	}
}

// List reads "name=url,name=url" from the account's clusters setting. When
// the account names a tunnel, every cluster is reached through it.
func (example) List(_ context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	if err := checkToken(req.Secrets); err != nil {
		return nil, err
	}
	spec := req.Account.ConfigString("clusters")
	if spec == "" {
		return nil, provider.Errorf(provider.InvalidConfig, "account %q has no clusters setting", req.Account.ID).
			WithHint("tug import example --account demo --set clusters=dev=https://127.0.0.1:6443")
	}
	var out []provider.Cluster
	for _, entry := range strings.Split(spec, ",") {
		name, server, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			return nil, provider.Errorf(provider.InvalidConfig, "bad cluster entry %q; want name=url", entry)
		}
		out = append(out, provider.Cluster{
			ID:        name,
			Name:      name,
			Endpoints: provider.Endpoints{Public: server},
			Labels:    map[string]string{"example": "true"},
			Tunnel:    req.Account.ConfigString("tunnel"),
		})
	}
	return out, nil
}

// Credential returns the account's token, which does not expire.
//
// With the authCommand setting it returns a delegate credential instead, as
// a provider whose vendor ships a kubectl exec plugin would: tug runs the
// command and uses the ExecCredential it prints. ProvideClusterInfo has tug
// tell the command which cluster it authenticates to, in the spec.cluster of
// KUBERNETES_EXEC_INFO, and ClusterConfig adds this cluster's settings as
// spec.cluster.config, as a kubeconfig's client.authentication.k8s.io/exec
// extension does. Leave both out for a command that does not read them.
func (example) Credential(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	if err := checkToken(req.Secrets); err != nil {
		return provider.Credential{}, err
	}
	if command := req.Account.ConfigString("authCommand"); command != "" {
		config, err := json.Marshal(map[string]string{"cluster": req.ClusterID})
		if err != nil {
			return provider.Credential{}, provider.Errorf(provider.Internal, "encoding the cluster config: %v", err)
		}
		c := provider.Delegate(command, "token")
		c.Exec.ProvideClusterInfo = true
		c.Exec.ClusterConfig = config
		return c, nil
	}
	return provider.Token(req.Secrets["api_token"], provider.NoExpiry), nil
}

func checkToken(secrets map[string]string) error {
	token := secrets["api_token"]
	if token == "" {
		return provider.Errorf(provider.Unauthenticated, "no api_token")
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		return provider.Errorf(provider.Unauthenticated, "api_token is not an example token").
			WithHint("example tokens start with " + tokenPrefix)
	}
	return nil
}

func main() {
	os.Exit(provider.Main(example{}))
}
