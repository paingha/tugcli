# tug

A CLI that makes Kubernetes clusters easy to reach and work with. tug finds
clusters across providers, keeps their credentials in the OS keychain, and
points each terminal at one cluster without touching `~/.kube/config`.

**Documentation:** [paingha.github.io/tugcli](https://paingha.github.io/tugcli/)
has the full user guide: installing, every provider and command, and writing
plugins.

> Status: early development. Phases 0 and 1 are in place, and most of v1:
> tugd (the local API proxy, tunnels to private clusters, named
> port-forwards), OIDC sign-in, credential rotation, directory pins, signed
> plugin indexes, plugin serve mode, a Python SDK and seventeen providers,
> as well as a TUI, commands across clusters, an audit log, and signed team
> catalogs with safety policies.

## Quick start

```bash
go build -o bin/tug ./cmd/tug   # or: make build
```

Add the shell hook to your rc file (after any prompt theme):

```bash
eval "$(tug init zsh)"          # bash: tug init bash, fish: tug init fish | source
```

The prompt then shows the terminal's cluster, namespace and env label, whether
it is read-only or how long an unlock has left, and `proxy` when it goes
through tugd. A production cluster gets a red badge in every mode, and an
unlock ends in the prompt on time, using the shell's own clock.

Then:

```bash
tug import                      # find kubeconfig files on this machine and import their clusters
tug import kubeconfig ./lke.yaml  # import a downloaded kubeconfig; its token moves to the keychain
tug ls                          # list clusters
tug connect prod                # fuzzy-pick a cluster for this terminal only
tug ns payments                 # switch namespace for this session
tug doctor                      # why can't I reach this cluster? DNS, TLS, credentials, clock, RBAC, access lists
tug logs api -f                 # deploy/api-server: every pod, coloured prefixes, new pods too
tug sh api                      # shell into a pod (bash, else sh; a debug container for distroless images)
tug why api                     # why it is not ready: crashes, image pulls, probes, scheduling, volumes
tug tree api                    # the workload, its ReplicaSets, pods, Services, config and volumes, with health
tug pick                        # fuzzy-pick an object, then why, tree, logs, describe or pf on it
tug bookmark add api deploy/api-server -n shop   # later, from any terminal: tug bookmark open api [logs|tree|pf]
tug open deploy/api             # the file in this repository that defines it, in $EDITOR (and as path:line)
tug alias set wide 'get -o wide'  # tug wide pods; an alias never hides tug's commands, or a write
tug why certificate/web-tls     # any kind: its conditions, whether its controller caught up, what it owns
tug diff -f k8s/                # what an apply would change, by server-side dry run, with each field's owner
tug diff api --against staging  # the same object on another cluster, minus what each cluster sets itself
tug drift --against staging     # the objects, CRDs and API versions that differ between two clusters
tug watch api --until ready     # follow the rollout live: conditions, replicas, pods, restarts; exit 0 when done
tug support bundle api          # a redacted archive of its YAML, why, events, logs and nodes, for a ticket
tug pf api 8080                 # port-forward by short name
tug pf db 5432 --name db        # in the background through tugd, reconnecting when the pod restarts
tug restart api | tug scale api 3 | tug rollback api
tug get pods                    # anything tug does not know runs as kubectl
tug unlock 15m                  # allow writes on a read-only (env=prod) session
tug auth rotate lke-prod        # new credential from the provider; the old one is revoked
tug auth login cks-train        # sign in to the cluster's OIDC issuer in the browser (--device over SSH)
tug tunnel set eks-private ssm://i-0123456789abcdef0   # reach a private cluster; see Tunnels
tug pin set payments-staging -n payments  # this directory pins the terminal to a cluster
tug profile up payments         # a saved environment: cluster, namespace, safety, aliases and forwards
tug plugin install digitalocean # from a signed plugin index
tug ui                          # full-screen: clusters, workloads, logs, why, tree, shells, forwards
tug get pods --cluster-selector env=staging   # any read, on every matching cluster at once
tug audit --since 24h           # what tug let through and refused, per cluster
tug team add platform https://teams.example.com/platform/team.yaml --public-key <key>  # shared names, labels and policies
```

Targets are fuzzy: exact beats prefix beats fuzzy, workloads beat bare pods,
and a picker opens on ties. Add a kind to be explicit: `sts/db`, `pod/debug`.

## Providers

| Provider | Ships as | Accounts | Credentials |
| --- | --- | --- | --- |
| `kubeconfig` | built-in | `~/.kube/config`, `$KUBECONFIG`, files you name | as in the file; embedded tokens and keys move to the keychain |
| `local` | built-in | one per tool found: kind and k3d (through their CLIs), minikube, Docker Desktop, OrbStack, Rancher Desktop (through their contexts) | the tool's client certificate, read fresh after a recreate; labelled `local=true`, `distro=<tool>`, `env=dev` |
| `aws` (EKS) | built-in | `~/.aws` profiles; `--set regions=us-east-1,eu-west-1` | short-lived token from a presigned STS request; no AWS CLI needed |
| `google` (GKE) | built-in | gcloud configurations (one account per project) | short-lived OAuth token from Application Default Credentials, else `gke-gcloud-auth-plugin` |
| `akamai` (LKE) | plugin, [plugins/akamai](plugins/akamai) | personal access token with Kubernetes read/write | the kubeconfig's token, kept in the keychain; `tug auth rotate` regenerates it |
| `coreweave` (CKS) | plugin, [plugins/coreweave](plugins/coreweave) | API access token | the API token (managed auth), or with `--set auth=oidc` an ID token from the cluster's OIDC issuer (`tug auth login`) |
| `azure` (AKS) | built-in | the subscriptions your Azure sign-in sees (`az login`, environment, workload or managed identity); none, silently, when nothing is set up | a short-lived Entra token for Entra-integrated clusters, else the user credential of a local-account cluster |
| `capi` (Cluster API) | built-in | a management cluster in the catalog: `--set cluster=<name>` | from each cluster's `<name>-kubeconfig` Secret, read again before it expires |
| `argocd` | built-in | an Argo CD install in the catalog: `--set cluster=<name>`, optional `namespace` | Argo CD's cluster Secrets: tokens, certificates, `aws eks get-token`; commands named by `execProviderConfig` run only with `--set allowExec=true` |
| `clusterprofile` | built-in | a SIG Multicluster hub in the catalog: `--set cluster=<name> --set providerFile=<path>` | the exec plugin your providers file names for each ClusterProfile's access provider |
| `digitalocean` (DOKS) | plugin, [plugins/digitalocean](plugins/digitalocean) | personal access token with `kubernetes:read` and `kubernetes:access_cluster` | a short-lived token from the credentials API |
| `vultr` (VKE) | plugin, [plugins/vultr](plugins/vultr) | API key | the cluster's client certificate |
| `rancher` | plugin, [plugins/rancher](plugins/rancher) | `--set url=<Rancher server>` and an API token | a kubeconfig token generated per cluster |
| `nebius` (mk8s) | plugin, [plugins/nebius](plugins/nebius) | the `nebius` CLI's profile, optional `project` | the CLI's `get-token`, run by kubectl |
| `crusoe` (CMK) | plugin, [plugins/crusoe](plugins/crusoe) | API access key and secret key, optional `project` | the cluster's OIDC issuer (`tug auth login`), else the admin certificate |
| `teleport` | plugin, [plugins/teleport](plugins/teleport) | your `tsh` sign-in, optional `proxy` and `cluster` | `tsh kube credentials`, run by kubectl; a `tsh://` tunnel when the proxy needs `tsh proxy kube` |
| `tailscale` | plugin, [plugins/tailscale](plugins/tailscale) | your tailnet: operator API server proxies tagged `tag:k8s-operator` | tailnet identity; reached at the device's MagicDNS name |

```bash
tug import local --account kind           # every kind cluster, even ones no kubeconfig mentions
tug import aws --account work --set regions=eu-west-1
tug import google --account my-project
gh release download --repo paingha/tugcli --pattern 'tug-provider-akamai_*' --pattern checksums.txt
tug plugin install tug-provider-akamai_*.tar.gz --sha256 "$(grep tug-provider-akamai_ checksums.txt | cut -d' ' -f1)"
tug import akamai --account work          # asks for the token once, keeps it in the keychain
tug import azure                          # every subscription your az login sees
tug import capi --account mgmt --set cluster=mgmt-eu
```

Each release includes the first-party plugins as archives with a build for
every platform they support, listed in the signed `checksums.txt`.
`make plugins-dist` builds the same archives from a checkout.

Every kubeconfig tug writes authenticates with `tug auth token`, so no secret is
written to disk. For clusters that trust an OpenID Connect issuer, tug signs in
with the authorization code flow and PKCE (the browser comes back to
`http://localhost:8000`, or `18000`, as with kubelogin) or with a device code,
keeps the refresh token in the keychain, and refreshes the ID token as it
expires. `oidc.listenAddresses` in `config.yaml` changes the redirect ports. kubectl, helm and k9s follow the session through `KUBECONFIG`.

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
without a live session or a named port-forward.

A proxied session fails closed: its kubeconfig names only tugd, so when tugd
is down clients fail rather than reach the cluster directly, and a restarted
tugd refuses the old tokens until clients fetch new ones. Expiring
credentials are replaced mid-session; one the cluster refuses is reported as
such, and tugd asks the provider for a new one. After the machine wakes from
sleep, tugd drops its idle connections to clusters. tugd is a safety rail, not
a security boundary: it sees only clients that use the session's kubeconfig
and only the credential tug keeps, so RBAC, cloud IAM, admission control and
the API server's audit log remain the enforcement (see
[What tugd protects](docs/content/docs/concepts/proxy-limits.mdx)).

tugd also keeps named port-forwards open: `tug pf <target> <ports> --name
<name>` hands one to tugd, which keeps the same local ports and starts the
forward again on a running pod when the old one goes away. `tug pf ls` lists
them, with their state, restarts and when a dropped one retries, and
`tug pf stop <name>|--all` ends them. `tug pf profile up|down|ls <profile>`
starts and stops a profile's forwards as a group; every local port is checked
first, and none start when one is taken.

## Tunnels

A cluster whose API server is not directly reachable gets a tunnel, kept in
the catalog across syncs. tug's own commands go through it, and connecting
to the cluster routes kubectl, helm and k9s through tugd, which keeps the
tunnel open. A provider can list a cluster with a tunnel, as the Teleport
plugin does for clusters that need `tsh proxy kube`; a tunnel you set wins
over it, and `tug tunnel ls` shows where each one comes from.

```bash
tug tunnel set eks-private 'ssm://i-0123456789abcdef0?region=eu-west-1'
tug tunnel set lab ssh://admin@bastion.example.com
tug tunnel set corp socks5://127.0.0.1:1080
tug tunnel test eks-private     # open it and reach the API server through it
tug tunnel ls | rm <cluster>
```

| Tunnel | How it works |
| --- | --- |
| `ssh://[user@]host[:port]` | an SSH jump host; keys from ssh-agent, `~/.ssh` or `?identity=`, and the host key must already be in `known_hosts` |
| `socks5://`, `http://`, `https://` | a proxy that needs no password, since tug keeps no secrets in its catalog |
| `ssm://<instance>[?region=&profile=&mux=true]` | an AWS Systems Manager session through a managed instance that can reach the private endpoint; tug speaks Session Manager itself, so it needs only AWS credentials, not the session-manager-plugin. KMS-encrypted sessions work, a dropped session resumes, and `mux=true` shares one session among connections on newer agents (by default each connection has its own, so one slow stream cannot stall the rest) |
| `iap://<instance>?project=&zone=` | Google Cloud IAP TCP forwarding to a VM; needs only Application Default Credentials, not gcloud |
| `iap://?project=&region=&network=&group=` | IAP to a host in a destination group, such as a private GKE endpoint; add `cert=` and `key=` (PEM files) for certificate-based access through the mTLS relay, and `universe=` for another Google Cloud universe |
| `tsh://<proxy>/<Teleport cluster>/<Kubernetes cluster>` | Teleport's `tsh proxy kube`, for a Teleport cluster that kubectl cannot reach directly: behind a load balancer that needs connection upgrades, or with a hardware key. tsh keeps the Teleport certificate; tug runs it, and uses the CA and client certificate of its local proxy, which change each time it starts |

Kubernetes clients honour a proxy for exec, attach and port-forward but not
a custom dialer, so tug serves SSH, SSM, IAP and tsh tunnels as a SOCKS5 proxy
on a loopback port, guarded by a random username and password that only tug
knows. `tug doctor` checks a tunnelled cluster through its tunnel.

## The TUI

`tug ui` is a full-screen view on the same core as the commands. The
clusters view filters as you type (by name, label value or `label=value`)
and `enter` connects this terminal, as `tug connect` does. The workloads
view lists the namespace's deployments, statefulsets, daemonsets, jobs and
pods with their health, refreshing every few seconds. On a workload, `l`
streams logs, `w` explains it, `t` shows its tree, `s` opens a shell (with a
debug container for images without one), `p` starts a named port-forward
in tugd, and `r` and `S` restart and scale, through the session's guard.
`n` switches namespace, `c` goes back to clusters and `?` lists every key.
`tug ui --k9s` launches k9s on the session instead.

## Shortcuts

`tug pick [kind]` is a full-screen fuzzy picker over the namespace's
objects, with each one's status, age and owner; choose one and run `why`,
`tree`, `logs`, `describe` or `pf` on it. `tug bookmark add <name> [target]`
saves an object, resolved exactly, or a cluster and namespace, and `tug
bookmark open <name> [action]` runs the action on it from any terminal,
through a temporary read-only session when it is on another cluster. `tug
open <target>` finds the file that defines an object, from kustomize's
origin annotation, the path of the Flux Kustomization or Argo CD Application
that applied it, its Helm chart or a search of the repository, and opens it
in `$VISUAL` or `$EDITOR`; without one, it opens the live YAML read-only.
`tug alias set <name> <expansion>` adds your own commands: an alias can't
hide one of tug's, and one that may write says what it runs and goes through
the guard. Bookmarks and aliases live in `bookmarks.yaml` and `aliases.yaml`
in tug's config directory.

## Across clusters

`--cluster-selector label=value` (repeatable; a cluster must match all)
runs a read on every matching cluster at once: any kubectl command that only
reads, and `tug why`, `tree`, `watch`, `logs` and `top`. Each line is prefixed with
the cluster's name, failures are listed at the end, and tug exits 1 if any
cluster failed. `--parallel` (default 8) caps how many run together. Writes,
and commands that bind a local port or the terminal, are refused before
anything runs. Tunnelled clusters go through their tunnels.

## Diffs and drift

`tug diff -f <file>` shows what an apply would change: it asks the API server
for a server-side dry run, so admission and defaulting show, and lists each
differing field with the field managers that own it, from managedFields,
marking the fields an apply would take from another manager. `tug diff
<target> --against <cluster>` compares an object with the same one on another
catalog cluster, leaving out what each cluster sets itself (allocated IPs,
autoscaled replicas, defaults), and `--managed-fields` lists who owns what.
`tug drift --against <cluster> [resource...]` compares a namespace's objects,
the CRDs and the API versions of two clusters. Both exit as `kubectl diff`
does, which `tug diff` takes the place of: 0 same, 1 different, 2 on an
error. Neither reads a Secret's data.

## Audit log

tug records in `<data>/audit/audit.jsonl` what it lets through and what it
refuses: writes by tug's own commands, by kubectl, helm or k9s through tugd,
and kubectl writes on direct sessions, where tug sees only the command line;
writes the guard refuses; sessions starting and ending on a cluster, with
the namespace, read-only or read-write, and whether they go through tugd;
and unlocks starting, being locked and running out. A write keeps apart the
intent (the tug or kubectl command, which kubectl names in a header), the
request tug or tugd saw (method and path), and the outcome (the HTTP status,
or kubectl's exit code). Entries never hold a request body, a query string, a
token or a command-line value that could be a secret. A write that cannot be
recorded is not sent, and an unlock that cannot be recorded is taken back.
`tug audit [--cluster x] [--session id] [-n ns] [--event write|refused|...]
[--since 24h] [-o json|yaml]` lists them, newest first. The log is a local
record kept with your own permissions, not evidence that nobody changed it.

## Team catalogs

A team publishes a signed catalog with names, aliases and labels for its
clusters, provider account settings, approved plugin indexes and safety
policies. It never carries credentials: each person imports the team's
accounts and reaches every cluster as themselves, through their own cloud or
OIDC sign-in, and publishing a catalog grants nobody any Kubernetes
permission.

```bash
tug team add platform https://teams.example.com/platform/team.yaml --public-key <key>
tug team add infra --git git@github.com:acme/tug-team.git --path infra.yaml --public-key <key>
tug team sync                   # fetch, check, show what changes, ask, apply
tug team ls | show platform | remove platform
tug team keygen | sign team.yaml | verify team.yaml --public-key <key>   # for publishers
```

The catalog is signed with ed25519, as plugin indexes are, and has a version
that must go up: tug refuses a bad signature, a lower version and a changed
catalog that kept its version, and keeps the last good version for offline
use. A sync shows what a new version changes before it applies (`--yes` skips
the question, and is needed without a terminal). Every entry records its team
and version, your own labels, names and accounts win over the team's, and
each conflict is listed. Teams can also be listed under `teams` in
`config.yaml`.

Policies map label selectors to read-only defaults and limits:

```yaml
policies:
- {name: production-read-only, selector: {env: prod}, readOnly: true, maxUnlock: 30m}
- {name: staging-write, selector: {env: staging}}
```

On a cluster under `production-read-only`, `tug unlock 2h` is refused,
`tug unlock` lasts at most 30 minutes, and `tug connect --read-write` is
refused. Policies combine with `guard.readOnlyLabels` so that the stricter
always wins, and they see a cluster's labels with the team's winning, so a
label set by hand cannot take a cluster out of one. Like the guard itself,
they are a seatbelt, not access control.

## Directory pins

A `.tug.yaml` pins a directory, and everything below it, to a cluster and
namespace. Moving into it with the shell hook connects the terminal as the
file says; leaving it goes back; connecting by hand ends the pin.

```yaml
cluster: payments-staging   # a name, prefix or fuzzy match, as for tug connect
namespace: payments
```

A pin file someone else wrote could switch your terminal on its own, so tug
uses one only after `tug pin allow`, and again only after any change to it is
allowed too. `tug pin set <cluster> -n <ns>` writes and allows one; `tug pin
show` and `tug pin revoke` check and withdraw. Set `TUG_PIN_DISABLE=1` to turn
pins off.

## Profiles

A profile is a YAML file in `profiles/` in tug's config directory that bundles
a cluster, a namespace, a safety mode, command aliases and named
port-forwards, so it can be reviewed like any other file:

```yaml
cluster: payments-prod
namespace: shop
safety: read-only           # or auto: the cluster's labels decide
proxy: true
aliases:
  api-logs: logs api -f     # tug profile run api-logs
forwards:
  - {name: db, target: sts/postgres, ports: [5432]}
```

`tug profile use <name>` connects the terminal as it says, `tug profile up`
also starts its forwards through tugd and `tug profile down` stops them.
`tug profile edit <name>` writes one in your editor and saves it only once it
parses (or reads it from stdin in a script); `ls`, `show` and `run <alias>` do
the rest. A profile can make a session read-only but never read-write: on a
cluster whose labels make it read-only, writes still ask first or need
`tug unlock`. Profiles are chosen only by name, never by a pin.

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
| tugd: local API proxy, named port-forwards, control socket | [internal/daemon](internal/daemon) |
| Tunnels: SSH, proxies, AWS SSM, Google IAP, Teleport's tsh | [internal/tunnel](internal/tunnel) |
| OpenID Connect sign-in | [internal/oidc](internal/oidc) |
| `.tug.yaml` pins and their trust | [internal/pin](internal/pin) |
| Team catalogs: signing, fetching, merging, policies | [internal/team](internal/team) |
| Working-environment profiles | [internal/profile](internal/profile) |
| Bookmarks and command aliases | [internal/bookmark](internal/bookmark), [internal/alias](internal/alias) |
| Finding the file that defines an object, and opening an editor | [internal/manifest](internal/manifest), [internal/editor](internal/editor) |
| Logs, exec, debug containers, port-forward, rollouts, why and tree (shared by CLI and TUI) | [internal/ops](internal/ops) |
| Diffs, server-side dry runs, field ownership and drift across clusters | [internal/diff](internal/diff) |
| Support bundles: collection, redaction and the archive | [internal/bundle](internal/bundle) |
| Tables, JSON/YAML, colour | [internal/render](internal/render) |
| First-party plugins (Akamai, CoreWeave, Crusoe, DigitalOcean, Nebius, Rancher, Tailscale, Teleport, Vultr) | [plugins](plugins) |
| Python SDK for plugins | [sdk/python](sdk/python) |
| Example plugin | [examples/tug-provider-example](examples/tug-provider-example) |

## Provider plugins

A plugin is an executable named `tug-provider-<name>` that reads one JSON
request on stdin and writes one JSON response on stdout, in any language.
[docs/plugins.md](docs/plugins.md) is the guide to writing, testing,
packaging and installing one, with the protocol and manifest reference. To
build a Go plugin against SDK changes that aren't on `main` yet, scaffold it
from a local checkout: `tug plugin new acme --sdk-replace <path to tugcli>`.

```bash
tug plugin new acme                    # Go, with the SDK, tests and a release workflow
tug plugin new acme --lang python      # one stdlib-only script, no SDK needed
cd tug-provider-acme && go mod tidy && make conformance

tug plugin package . -o dist           # archive with each binary's sha256 in the manifest
tug plugin search gpu                  # the signed plugin indexes in plugins.indexes
tug plugin install acme                # by name from an index, or acme@0.1.0
tug plugin install ./dist/tug-provider-acme_0.1.0.tar.gz
tug plugin install https://.../tug-provider-acme_0.1.0.tar.gz --sha256 <archive sha256>
tug plugin test acme --account prod    # protocol conformance suite
tug plugin ls | describe | link | unlink | remove
```

Install verifies this platform's binary against the manifest, refuses
archives with links or paths outside the plugin directory, and shows the
plugin's publisher, secrets and network hosts before asking to confirm.
Plugins run with a scrubbed environment and receive only the secrets they
declare, inside an OS sandbox: Landlock and seccomp on Linux,
`sandbox-exec` on macOS. A plugin reads only its own directory, system
files and the paths its manifest's `spec.sandbox` declares, writes only a
private temporary directory, runs only its declared tools, and connects
only to port 443 and the ports it declares. Windows has no sandbox. A
plugin that needs more goes in `plugins.sandbox.exclude`, and
`plugins.sandbox.enabled: false` turns it off. A plugin whose `minTugVersion` is newer than tug is refused. Fields
in `plugin.yaml` that tug does not know are ignored with a warning, and
`tug import` warns about `--set` keys the plugin's `accountConfig` does not
describe. `plugins.timeouts` in `config.yaml` sets the time allowed for each
operation, for example `{list: 60s, rotate: 2m}`.

A plugin index lists reviewed plugins with each archive's URL and sha256 and
is signed with ed25519, so installing by name checks the index's signature,
the archive's sha256 and every binary's sha256. An entry can also name a
Sigstore bundle and the identity that must have signed the archive, such as
the publisher's release workflow; tug then checks the signature against the
public Sigstore logs before installing, and `plugins.requireSignatures: true`
refuses entries without one. Add an index to
`plugins.indexes` in `config.yaml` with its `name`, `url` and `publicKey`;
there is no default until one is published. Publishers use
`tug plugin index keygen | add | sign | verify`.

Plugins that declare the `serve` capability, as every SDK plugin can, run as
one process that answers many requests over stdio, so tugd and commands that
make several calls do not start a process each time.

A plugin can also ship a WebAssembly build (`{os: wasip1, arch: wasm}`),
which tug runs itself with wazero: one file for every platform, no files but
a private temporary directory, and https only to the hosts in
`spec.network`, through tug. The Go scaffold's `make wasm` builds one; tug
prefers a native build unless `plugins.wasm.prefer` is set, and
`plugins.wasm.memoryLimitMiB` caps memory (256 by default). `tug plugin
describe` says how a plugin runs.

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
plugin. Python plugins can use [sdk/python](sdk/python), the `tug_provider`
package, which has no dependencies.

## Development

```bash
make test      # unit tests and in-process end-to-end tests (fake API server)
make lint      # golangci-lint
make e2e-kind  # build, create a kind cluster, run scripts/e2e-kind.sh
```

CI runs the tests on Linux, macOS and Windows, and the end-to-end kind test
on Linux.

Tests never touch your keychain: they set `TUG_HOME` to a temporary directory
and use the encrypted file store (`TUG_KEYRING_BACKEND=file`,
`TUG_KEYRING_PASSPHRASE`). The same variables work in CI and on servers without
a keychain.

Code follows [Effective Go](https://go.dev/doc/effective_go),
[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) and the
[Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md).
[AGENTS.md](AGENTS.md) holds the Go and CLI standards for this repository,
for people and for AI coding agents.

## Contributing

Bug reports, docs fixes, plugins and pull requests are welcome.
[CONTRIBUTING.md](CONTRIBUTING.md) explains how to report a problem, set up a
development copy and get a change merged.

## License

tug is released under the [MIT License](LICENSE).
