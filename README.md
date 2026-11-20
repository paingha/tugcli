# tug

A CLI that makes Kubernetes clusters easy to reach and work with. tug finds
clusters across providers, keeps their credentials in the OS keychain, and
points each terminal at one cluster without touching `~/.kube/config`.

> Status: early development. Phase 0 (foundations) and most of Phase 1 are in
> place: sessions, catalog, credentials, the plugin protocol and host,
> guardrails, the daily commands, the plugin tooling, `doctor`, and the EKS,
> GKE, Akamai LKE and CoreWeave CKS providers. Local clusters (kind, k3d,
> minikube) as their own provider are next.

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
tug doctor                      # why can't I reach this cluster? DNS, TLS, credentials, clock, RBAC, access lists
tug logs api -f                 # deploy/api-server: every pod, coloured prefixes, new pods too
tug sh api                      # shell into a pod (bash, else sh)
tug pf api 8080                 # port-forward by short name
tug restart api | tug scale api 3 | tug rollback api
tug get pods                    # anything tug does not know runs as kubectl
tug unlock 15m                  # allow writes on a read-only (env=prod) session
```

Targets are fuzzy: exact beats prefix beats fuzzy, workloads beat bare pods,
and a picker opens on ties. Add a kind to be explicit: `sts/db`, `pod/debug`.

## Providers

| Provider | Ships as | Accounts | Credentials |
| --- | --- | --- | --- |
| `kubeconfig` | built-in | `~/.kube/config`, `$KUBECONFIG`, files you name | as in the file; embedded tokens and keys move to the keychain |
| `aws` (EKS) | built-in | `~/.aws` profiles; `--set regions=us-east-1,eu-west-1` | short-lived token from a presigned STS request; no AWS CLI needed |
| `google` (GKE) | built-in | gcloud configurations (one account per project) | short-lived OAuth token from Application Default Credentials, else `gke-gcloud-auth-plugin` |
| `akamai` (LKE) | plugin, [plugins/akamai](plugins/akamai) | personal access token with Kubernetes read/write | the kubeconfig's token, kept in the keychain; `rotate` regenerates it |
| `coreweave` (CKS) | plugin, [plugins/coreweave](plugins/coreweave) | API access token | the API token (managed auth), or the cluster's OIDC issuer with `--set auth=oidc` |

```bash
tug import aws --account work --set regions=eu-west-1
tug import google --account my-project
make plugins && tug plugin link plugins/akamai
tug import akamai --account work          # asks for the token once, keeps it in the keychain
```

Every kubeconfig tug writes authenticates with `tug auth token`, so no secret is
written to disk. kubectl, helm and k9s follow the session through `KUBECONFIG`.

## The local API proxy (tugd)

Without the proxy, a read-only session protects tug's own commands, but
kubectl, helm and k9s talk to the cluster directly. With `--proxy`, every
client goes through tugd, a background process on this machine, and the guard
applies to all of them:

```bash
tug connect prod-eu --proxy        # starts tugd if needed
kubectl delete pod api-0           # Error from server (Forbidden): tug: refused DELETE pods/api-0: prod-eu is read-only
tug unlock 15m                     # takes effect on the next request, for every client
tug daemon status | stop
```

tugd serves TLS on a loopback port and holds each cluster's real credential.
A session's kubeconfig trusts tugd's certificate and authenticates with a
per-session token that exists only in tugd's memory, fetched through
`tug auth proxy-token`, so nothing secret is written to disk. Exec, attach,
port-forward, `logs -f` and watches pass through. tugd stops after 30 minutes
without a live session.

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
| Plugin host: discovery, manifests, runner, install, scaffolding | [internal/plugin](internal/plugin) |
| Protocol conformance suite (public) | [pkg/conformance](pkg/conformance) |
| Client factory and discovery cache | [internal/kube](internal/kube) |
| Read-only guard as an `http.RoundTripper` | [internal/guard](internal/guard) |
| kubectl passthrough | [internal/passthrough](internal/passthrough) |
| Fuzzy target and workload resolution | [internal/resolve](internal/resolve) |
| Diagnostics behind `tug doctor` | [internal/doctor](internal/doctor) |
| tugd: local API proxy, control socket | [internal/daemon](internal/daemon) |
| Logs, exec, port-forward, rollouts (shared by CLI and TUI) | [internal/ops](internal/ops) |
| Tables, JSON/YAML, colour | [internal/render](internal/render) |
| First-party plugins (Akamai LKE, CoreWeave CKS) | [plugins](plugins) |
| Example plugin | [examples/tug-provider-example](examples/tug-provider-example) |

## Provider plugins

A plugin is an executable named `tug-provider-<name>` that reads one JSON
request on stdin and writes one JSON response on stdout, in any language.

```bash
tug plugin new acme                    # Go, with the SDK, tests and a release workflow
tug plugin new acme --lang python      # one stdlib-only script, no SDK needed
cd tug-provider-acme && make conformance

tug plugin package . -o dist           # archive with each binary's sha256 in the manifest
tug plugin install ./dist/tug-provider-acme_0.1.0.tar.gz
tug plugin install https://.../tug-provider-acme_0.1.0.tar.gz --sha256 <archive sha256>
tug plugin test acme --account prod    # protocol conformance suite
tug plugin ls | describe | link | unlink | remove
```

Install verifies every binary against the manifest, refuses archives with
links or paths outside the plugin directory, and shows the plugin's
publisher, secrets and network hosts before asking to confirm. Plugins run
with a scrubbed environment and receive only the secrets they declare.

With the Go SDK, a plugin is mostly two methods:

```go
func (acme) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) { ... }
func (acme) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) { ... }

func main() { os.Exit(provider.Main(acme{})) }
```

The conformance suite is a public package, [pkg/conformance](pkg/conformance),
so plugin authors can run it in their own tests; tug's CI runs it against the
built-in providers through the same process interface. See
[examples/tug-provider-example](examples/tug-provider-example) for a complete
plugin.

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
[AGENTS.md](AGENTS.md) holds the Go and CLI standards for this repository,
for people and for AI coding agents.
