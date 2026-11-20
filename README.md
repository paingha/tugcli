# tug

A CLI that makes Kubernetes clusters easy to reach and work with. tug finds
clusters across providers, keeps their credentials in the OS keychain, and
points each terminal at one cluster without touching `~/.kube/config`.

> Status: early development. Phase 0 (foundations) and most of Phase 1 are in
> place: sessions, catalog, credentials, the plugin protocol and host,
> guardrails, and the daily commands. Cloud providers (EKS, GKE, Akamai LKE,
> CoreWeave CKS), `doctor` and the plugin tooling (`install`, `new`, `test`)
> are next.

## Quick start

```bash
go build -o bin/tug ./cmd/tug   # or: make build
```

Add the shell hook to your rc file (after any prompt theme):

```bash
eval "$(tug init zsh)"          # bash: tug init bash, fish: tug init fish | source
```

Then:

```bash
tug import                      # find kubeconfig files on this machine and import their clusters
tug import kubeconfig ./lke.yaml  # import a downloaded kubeconfig; its token moves to the keychain
tug ls                          # list clusters
tug connect prod                # fuzzy-pick a cluster for this terminal only
tug ns payments                 # switch namespace for this session
tug logs api -f                 # deploy/api-server: every pod, coloured prefixes, new pods too
tug sh api                      # shell into a pod (bash, else sh)
tug pf api 8080                 # port-forward by short name
tug restart api | tug scale api 3 | tug rollback api
tug get pods                    # anything tug does not know runs as kubectl
tug unlock 15m                  # allow writes on a read-only (env=prod) session
```

Targets are fuzzy: exact beats prefix beats fuzzy, workloads beat bare pods,
and a picker opens on ties. Add a kind to be explicit: `sts/db`, `pod/debug`.

Every kubeconfig tug writes authenticates with `tug auth token`, so no secret is
written to disk. kubectl, helm and k9s follow the session through `KUBECONFIG`.

## How it fits together

| Piece | Where |
| --- | --- |
| Entry point (`os.Exit(cli.Main())` only) | [cmd/tug](cmd/tug/main.go) |
| Public plugin SDK and wire protocol | [pkg/provider](pkg/provider) |
| Cobra commands, no logic | [internal/cli](internal/cli) |
| Per-terminal sessions and shell hooks | [internal/session](internal/session) |
| Cluster catalog, labels, stale marking | [internal/catalog](internal/catalog) |
| Import and sync of provider accounts | [internal/importer](internal/importer) |
| Keychain, encrypted file store, credential broker | [internal/credentials](internal/credentials) |
| Provider registry | [internal/registry](internal/registry) |
| Built-in providers | [internal/builtin](internal/builtin) |
| Plugin host: discovery, manifests, process runner | [internal/plugin](internal/plugin) |
| Client factory and discovery cache | [internal/kube](internal/kube) |
| Read-only guard as an `http.RoundTripper` | [internal/guard](internal/guard) |
| kubectl passthrough | [internal/passthrough](internal/passthrough) |
| Fuzzy target and workload resolution | [internal/resolve](internal/resolve) |
| Logs, exec, port-forward, rollouts (shared by CLI and TUI) | [internal/ops](internal/ops) |
| Tables, JSON/YAML, colour | [internal/render](internal/render) |
| Example plugin | [examples/tug-provider-example](examples/tug-provider-example) |

## Writing a provider plugin

A plugin is an executable named `tug-provider-<name>` that reads one JSON
request on stdin and writes one JSON response on stdout. With the Go SDK:

```go
type myProvider struct{}

func (myProvider) Info() provider.Info { return provider.Info{Name: "my"} }
func (myProvider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) { ... }
func (myProvider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) { ... }

func main() { os.Exit(provider.Main(myProvider{})) }
```

See [examples/tug-provider-example](examples/tug-provider-example), then:

```bash
make example-plugin
tug plugin link examples/tug-provider-example
tug plugin describe example
```

## Development

```bash
make test      # unit tests and in-process end-to-end tests (fake API server)
make lint      # golangci-lint
make e2e-kind  # build, create a kind cluster, run scripts/e2e-kind.sh
```

CI runs the tests on Linux and Windows. macOS is not in CI to save Actions
minutes, so run `make test` on a Mac before pushing changes that touch
platform-specific code (keychain, sessions, process handling).

Tests never touch your keychain: they set `TUG_HOME` to a temporary directory
and use the encrypted file store (`TUG_KEYRING_BACKEND=file`,
`TUG_KEYRING_PASSPHRASE`). The same variables work in CI and on servers without
a keychain.

Code follows [Effective Go](https://go.dev/doc/effective_go),
[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) and the
[Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md).
