# Writing a tug provider plugin

This guide takes you from nothing to a provider plugin that tug can install
and use: written, tested, packaged and installed. With the scaffold it should
take well under an hour. It is written for developers outside the tug team;
everything here matches the code in this repository, which remains the source
of truth.

> **The repository is private for now.** `github.com/paingha/tugcli` is not
> public yet, so `go get github.com/paingha/tugcli/pkg/provider` does not work
> for most people, and neither does `go install .../cmd/tug@latest`. Until it
> is public, you need a checkout of tugcli (ask the tug team for access or a
> source archive). You build tug from it, and point your plugin's Go module at
> it with `tug plugin new --sdk-replace <path to the checkout>`, as the quick
> start shows. There is no public module URL yet; do not guess one.

Contents:

1. [What a provider plugin is](#1-what-a-provider-plugin-is)
2. [Before you start](#2-before-you-start)
3. [Quick start in Go](#3-quick-start-in-go)
4. [Implementing List and Credential well](#4-implementing-list-and-credential-well)
5. [Optional operations](#5-optional-operations)
6. [Protocol reference](#6-protocol-reference)
7. [Manifest reference](#7-manifest-reference)
8. [Secrets and the environment](#8-secrets-and-the-environment)
9. [Testing](#9-testing)
10. [Packaging, releasing and installing](#10-packaging-releasing-and-installing)
11. [Plugins without the SDK](#11-plugins-without-the-sdk)
12. [Troubleshooting](#12-troubleshooting)

## 1. What a provider plugin is

tug keeps a catalog of Kubernetes clusters and connects each terminal to one
of them. A *provider* is anything that can list clusters and hand out
credentials for them. tug has a few built in (`kubeconfig`, `aws`, `google`);
everything else is a plugin.

A plugin is an executable named `tug-provider-<name>` with a manifest,
`plugin.yaml`, beside it. For each operation, tug starts the executable with
the operation as its only argument, writes one JSON request to its stdin and
reads one JSON response from its stdout. Anything the plugin writes to stderr
goes to tug's log. The process then exits; nothing stays running. Any language
works. Go has an SDK, [`pkg/provider`](../pkg/provider), which does the
protocol for you.

Write a plugin when your clusters come from an API (a cloud, an internal
platform, a cluster registry) and you want tug to find them and fetch their
credentials. If you only have a kubeconfig file, you do not need one:
`tug import kubeconfig ./file.yaml` already imports it.

### How tug uses a plugin

```text
tug import acme --account work     tug sync
        |                              |
        +----------- list -------------+   clusters (and any inline credentials)
                       |
                 tug's catalog             names, endpoints, CA, labels; never secrets
                       |
tug connect dev  -->  session kubeconfig   its user runs `tug auth token --cluster <id>`
                       |
kubectl get pods --> tug auth token  -->  cached credential, or `credential` on a miss
```

1. **Import.** `tug import <name> --account <id>` asks for the secrets your
   plugin declares (once; they go to the OS keychain), then runs `list`. Each
   cluster goes into the catalog under the ID `<name>/<account>/<cluster id>`.
   Credentials you return inline with a cluster go straight to the keychain.
2. **Sync.** `tug sync` runs `list` again for every imported account. Clusters
   that are no longer listed are marked stale, not deleted.
3. **Connect.** `tug connect` writes a per-terminal kubeconfig whose user is an
   exec entry that runs `tug auth token`. No secret is written to disk.
4. **Credentials.** When kubectl, helm or k9s needs a credential, they run
   `tug auth token`. tug answers from the keychain while the cached credential
   is valid and runs your plugin's `credential` operation when it is missing
   or about to expire.

`list` and `credential` are required. `accounts`, `rotate` and `checkAccess`
are optional (see [section 5](#5-optional-operations)).

## 2. Before you start

You need:

- Go 1.26 or later (the scaffold's `go.mod` says `go 1.26.0`).
- `make` and a Unix shell, for the scaffold's Makefile. The commands below
  are for macOS and Linux; on Windows, use WSL or Git Bash.
- A checkout of tugcli, for example in `~/src/tugcli`, and tug built from it
  and on your `PATH`. The scaffold's Makefile runs `tug`.

```bash
cd ~/src/tugcli
make build                       # or: go build -o bin/tug ./cmd/tug
export PATH="$PWD/bin:$PATH"
tug version
```

While you experiment, keep tug away from your real catalog and keychain. These
variables give it a throwaway home and an encrypted file instead of the OS
keychain; tug's own tests do the same:

```bash
export TUG_HOME="$(mktemp -d)"
export TUG_KEYRING_BACKEND=file
export TUG_KEYRING_PASSPHRASE=dev-only
```

Unset them (or open a new terminal) when you want to use your plugin for real.

## 3. Quick start in Go

### Generate a project

Run this where you keep your code, not inside the tugcli checkout:

```bash
tug plugin new acme --sdk-replace ~/src/tugcli
cd tug-provider-acme
go mod tidy
```

`--sdk-replace` is a hidden flag: it adds a `require` and a `replace` directive
to `go.mod` so the SDK comes from your checkout. Without it, `go mod tidy`
tries to download `github.com/paingha/tugcli`, which fails while the
repository is private. If you forgot the flag, add the two lines to `go.mod`
yourself, with the absolute path to your checkout:

```text
require github.com/paingha/tugcli v0.0.0

replace github.com/paingha/tugcli => /home/you/src/tugcli
```

Pass `--module github.com/you/tug-provider-acme` to set your module path (the
default is `example.com/tug-provider-acme`), `--dir` to choose another
directory (the default is `./tug-provider-<name>`), or `--lang python` for a
Python plugin ([section 11](#11-plugins-without-the-sdk)).

The plugin name must be lower-case letters, digits and dashes, start and end
with a letter or digit, and be at most 63 characters. It must not be the name
of a built-in provider (`kubeconfig`, `aws`, `google`).

You get:

| File | What it is |
| --- | --- |
| `main.go` | The plugin: `Info`, `List`, `Credential` and `main`. |
| `main_test.go` | Unit tests that run each operation in process through `provider.Run`. |
| `plugin.yaml` | The manifest: name, version, platforms, secrets, settings. |
| `Makefile` | `build`, `build-all`, `test`, `link`, `conformance`, `package`, `clean`. |
| `.github/workflows/release.yml` | Builds and publishes an archive when you push a `v*` tag. |
| `.gitignore` | Ignores `bin/` and `dist/`. |
| `README.md` | A short README for your plugin. |

The generated plugin already works: it lists the clusters named in the
account's `clusters` setting (`name=url` pairs, comma separated) and accepts
any `api_token` that starts with `demo_`. That lets you try every step below
before you write any code, then replace the demo parts with calls to your API.

### Test, link and check it

```bash
make test          # go test ./...
make link          # build bin/<os>_<arch>/tug-provider-acme and run `tug plugin link .`
make conformance   # run tug's protocol conformance suite against the linked plugin
```

`tug plugin link .` registers the directory as a development plugin; it
refuses a directory whose binary for this platform is not built yet, which is
why `make link` builds first. tug runs the binary straight from `bin/`,
without checking its checksum, so after each `make build` (or `make link`) tug
uses the new build at once. `tug plugin ls` shows it with the kind
`dev plugin`.

`make conformance` runs:

```bash
TUG_SECRET_API_TOKEN=demo_test tug plugin test acme --set clusters=dev=https://127.0.0.1:6443
```

Every check should say `pass` or `skip`. [Section 9](#9-testing) explains each
one.

### Import it

```bash
tug import acme --account demo --set clusters=dev=https://127.0.0.1:6443
```

tug asks for the `acme API token`; type anything that starts with `demo_`,
such as `demo_test`. It keeps the token in the keychain (here, the encrypted
file) and runs `list`:

```text
✓ acme/demo: 1 clusters, 1 new
```

Then:

```bash
tug ls                           # dev is in the catalog, with env=dev
tug auth token --cluster dev     # the ExecCredential kubectl would receive
```

```text
  NAME   PROVIDER   ACCOUNT   REGION   VERSION   ENV   LABELS   STATUS
  dev    acme       demo      -        -         dev   -
{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1","spec":{"interactive":false},"status":{"token":"demo_test"}}
```

To use the cluster from kubectl, set up tug's shell hook as the
[README](../README.md#quick-start) shows and run `tug connect dev`. The demo
cluster points at `https://127.0.0.1:6443`, so kubectl only reaches something
if a cluster is listening there (a local kind cluster, for example). With a
real API, the endpoints are real.

Without a terminal (in a script or CI), tug cannot ask for the token. Supply it
in the environment instead:

```bash
TUG_SECRET_API_TOKEN=demo_test tug import acme --account demo --set clusters=dev=https://127.0.0.1:6443
```

A secret from the environment is used but not stored. The credentials tug
fetches with it are cached in the keychain, so `tug auth token` keeps working
until they expire, but `tug sync` and `tug auth token --refresh` need the
variable again. The rest of this walkthrough assumes you typed the token at
the prompt; if you used the variable, keep it set.

### Make a change

In `main.go`, `List` appends a `provider.Cluster` for each entry. Give it a
region and a second label by replacing that `append` call with:

```go
out = append(out, provider.Cluster{
	ID:        name, // must stay the same for the life of the cluster
	Name:      name,
	Region:    "eu-west",
	Endpoints: provider.Endpoints{Public: server},
	Labels:    map[string]string{"env": "dev", "team": "platform"},
})
```

At the end of `TestList` in `main_test.go`, check the new label:

```go
if clusters[0].Labels["team"] != "platform" {
	t.Fatalf("labels = %v, want team=platform", clusters[0].Labels)
}
```

Then test, rebuild and refresh the catalog:

```bash
make test
make link          # rebuilds; the link already points here
tug sync           # runs list again for every imported account
tug ls
```

```text
✓ acme/demo: 1 clusters
  NAME   PROVIDER   ACCOUNT   REGION    VERSION   ENV   LABELS          STATUS
  dev    acme       demo      eu-west   -         dev   team=platform
```

### Package and install it

A linked plugin is for development. To try what your users will get, build an
archive and install it:

```bash
make package       # builds every platform, then: tug plugin package . -o dist
```

```text
dist/tug-provider-acme_0.1.0.tar.gz
  platforms: darwin/arm64, darwin/amd64, linux/amd64, linux/arm64, windows/amd64
  sha256:    98b07c5943ae2674b04199c8f1f33cd24fb7ed56b8c03d6f0f9951dcc99b2ee9
```

A linked plugin takes precedence over an installed one of the same name, so
unlink it first, then install with the sha256 that `make package` printed:

```bash
tug plugin unlink acme
tug plugin install ./dist/tug-provider-acme_0.1.0.tar.gz --sha256 <sha256 from make package>
tug plugin ls      # acme is now of kind "plugin", in tug's data directory
tug sync           # the imported account now runs through the installed copy
```

`tug plugin install` shows the plugin's publisher, secrets, network hosts and
platforms and asks you to confirm; without a terminal, add `--yes`. To go
back to developing, run `make link` again.

From here, replace the TODOs in `main.go` with calls to your provider's API,
and fill in `publisher`, `homepage` and `network` in `plugin.yaml`. The next
section says what tug expects from `List` and `Credential`.

## 4. Implementing List and Credential well

A Go plugin implements `provider.Provider` and hands it to `provider.Main`.
The scaffold's `main.go` has this shape:

```go
type acmeProvider struct{}

func (acmeProvider) Info() provider.Info { ... }
func (acmeProvider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) { ... }
func (acmeProvider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) { ... }

func main() { os.Exit(provider.Main(acmeProvider{})) }
```

`Info` returns the name, version and declared secrets, which must match
`plugin.yaml`. `Description` says in a few words what the provider reaches;
`describe` sends it, and `tug plugin describe` shows it.

`provider.Main` reads the operation from `os.Args`, decodes the request,
calls your method, writes the response or error, and returns the exit code.
SIGINT and SIGTERM cancel the context it passes you.

Both requests carry the account (`req.Account.ID` and its settings in
`req.Account.Config`) and the secrets you declared (`req.Secrets`, by name).
`CredentialRequest` also carries `ClusterID`, the `ID` you returned from
`List`.

### Account settings

Users set account settings with `--set key=value` on `tug import`; tug stores
them with the account and sends them in every request. Values from `--set`
are always strings, so read them with `req.Account.ConfigString("key")`, which
returns `""` when the setting is missing. Settings from your own `accounts`
operation can be any JSON. Describe your settings in the manifest's
`accountConfig` ([section 7](#7-manifest-reference)).

A common setting is an API endpoint override, so users (and your tests) can
point the plugin at a regional or fake API. The first-party plugins call it
`apiURL` and accept only `https://` URLs, or `http://` on a loopback address
for tests; see [`plugins/internal/apiclient`](../plugins/internal/apiclient).

### Clusters

| Field | JSON | What tug does with it |
| --- | --- | --- |
| `ID` | `id` | Required. Part of the catalog ID `<provider>/<account>/<id>` and sent back as `clusterID`. Must be unique in the account and stable for the life of the cluster. |
| `Name` | `name` | Required. What users type. If another cluster already has the name, tug uses `name@account`, then `name@provider-account`. tug keeps the name it chose at first import. |
| `Endpoints.Public` / `Endpoints.Private` | `endpoints.public` / `endpoints.private` | At least one is required, as a URL with a scheme and host. tug connects to the public endpoint, or the private one when there is no public one. |
| `CAData` | `caData` | The cluster CA in PEM (base64 on the wire). Leave it empty when the API server has a publicly trusted certificate. |
| `Region` | `region` | Shown in `tug ls` and copied, in lower case, to the `region` label. |
| `Version` | `version` | The Kubernetes version, shown in `tug ls`. |
| `Labels` | `labels` | See below. |
| `TLSServerName` | `tlsServerName` | Server name to verify when it differs from the endpoint's host. |
| `ProxyURL` | `proxyURL` | A proxy to reach the API server through. |
| `DefaultNamespace` | `defaultNamespace` | The namespace a new session starts in. |
| `InsecureSkipTLSVerify` | `insecureSkipTLSVerify` | Turns off certificate checks. Avoid it; return `CAData` instead. |
| `Credential` | `credential` | An inline credential (see below). Never written to the catalog. |

Get these right:

- **Stable IDs.** Use the provider's own immutable ID, not the display name.
  If an ID changes, tug sees a new cluster and marks the old one stale, and
  the user loses the labels they set with `tug label`.
- **Complete listings.** If you cannot list everything, return an error rather
  than a partial list. A cluster missing from a successful listing is marked
  stale.
- **Clusters that are not ready.** A cluster still being created may have no
  endpoint yet. Skip it (and say so on stderr); the next `tug sync` picks it
  up. A cluster with no endpoint fails the conformance suite.
- **CA.** If the listing cannot fetch the CA this time, leave `CAData` empty:
  when the endpoint is unchanged, tug keeps the CA it already trusts.

### Labels

Labels are free-form `key: value` strings. `tug ls -l key=value` filters on
them, and users add their own with `tug label`; theirs win over yours. tug
lower-cases label keys and gives these ones meaning:

| Label | Meaning |
| --- | --- |
| `env` | The environment. tug normalises it: `production`, `prd` and `live` become `prod`; `stage` and `stg` become `staging`; `development` becomes `dev`; `qa` becomes `test`. **Clusters with `env=prod` open read-only by default**, so writes need `tug unlock`. If you set no `env`, tug guesses one from words in the cluster name, such as `payments-prod-eu`. |
| `gpu` | `true` when the cluster has GPU nodes. tug lower-cases the value. Filter with `tug ls -l gpu=true`. |
| `region` | Set by tug from `Region`; do not set it yourself. |
| `provider`, `account` | Set by tug; anything you return is overwritten. |

Map your provider's tags to labels when they are `key:value` or `key=value`
pairs, as the Akamai plugin does.

### Credentials

Return one of four shapes. The SDK has a constructor for each:

| Shape | Constructor | Use it when |
| --- | --- | --- |
| `token` | `provider.Token(token, expiresAt)` | The API server accepts a bearer token: an API token, a short-lived token you mint, a service account token. |
| `clientCert` | `provider.ClientCert(certPEM, keyPEM, expiresAt)` | The cluster uses client certificates. |
| `delegate` | `provider.Delegate(command, args...)` | The vendor already ships a kubectl exec credential plugin. tug runs it unchanged and, when its output has an expiry, caches it until then. |
| `oidc` | `provider.OIDC(issuer, clientID, scopes...)` | The cluster trusts an OpenID Connect issuer. tug signs the user in (`tug auth login`, or on its own when kubectl needs a token and stderr is a terminal), keeps the refresh token, and hands kubectl the ID token. `openid` is always requested; add `offline_access` if your issuer issues refresh tokens only with it. Set `ClientSecret` only for issuers that require one for public clients, as Google does. The issuer must accept `http://localhost:8000` and `http://localhost:18000` as redirect URIs, as for kubelogin. |

Set the expiry for anything short-lived. tug reuses a cached credential until
60 seconds before `ExpiresAt` and then asks your plugin again. Pass
`provider.NoExpiry` only for credentials that really do not expire: tug never
asks again for those unless the user runs `tug auth token --refresh` or a
listing returns a new one inline. A credential that is already expired fails
the conformance suite.

A delegated command runs with the user's full environment (not the scrubbed
one plugins get), and with the terminal when kubectl says the session is
interactive, so it can prompt. Set `Exec.InstallHint` to tell users how to
install it when it is missing.

**Inline credentials.** If your API returns the credential with the cluster,
as LKE does with a kubeconfig, set `Cluster.Credential` in `List`. tug moves
it to the keychain at import and sync, so the first `tug auth token` needs no
plugin call. You still implement `Credential` for when the cache is empty or
expired.

### Errors

Return a `*provider.Error` so tug can tell the user what went wrong and what to
do. Any other error reaches tug as `Internal`.

```go
return nil, provider.Errorf(provider.Unauthenticated, "the API rejected the token").
	WithHint("create a token with cluster read access at https://console.acme.example/tokens")
```

| Code | Use it when |
| --- | --- |
| `Unauthenticated` | The secrets are missing, wrong, expired or revoked (HTTP 401). The conformance suite requires this code for invalid secrets. |
| `PermissionDenied` | The identity is valid but not allowed (403). |
| `NotFound` | The account or cluster does not exist (404). |
| `RateLimited` | Slow down (429). Set `RetryAfterSeconds` when the API says how long. |
| `Unavailable` | The API cannot be reached, timed out, or failed (5xx). |
| `InvalidConfig` | The account settings or the request are wrong (400, 422, or a bad `--set`). |
| `Unimplemented` | The operation is not supported. |
| `Internal` | Anything else. |

tug prints the code and message, and the hint on a line of its own. For
example, `tug auth token` prints:

```text
tug: credentials for dev: Unauthenticated: invalid api_token
hint: until the plugin calls a real API, tokens must start with demo_
```

and `tug import` and `tug sync` print the hint under the account that failed.
Write the hint as the next step the user should take. tug does not retry or
back off on its own yet, so `RetryAfterSeconds` tells the user how long to
wait.

For HTTP APIs, `provider.StatusError(status, message, header)` does the
mapping in the table: 401 to `Unauthenticated`, 403 to `PermissionDenied`, 404
to `NotFound`, 429 to `RateLimited` (with `RetryAfterSeconds` from a
`Retry-After` header in seconds), 408 and 5xx to `Unavailable`, 400 and 422 to
`InvalidConfig`, and anything else to `Internal`.

```go
if resp.StatusCode < 200 || resp.StatusCode > 299 {
	// apiMessage is your own function that reads the API's error body.
	return nil, provider.StatusError(resp.StatusCode, "GET /v1/clusters: "+apiMessage(body), resp.Header)
}
```

Some APIs answer 403 for a token that is invalid rather than merely
underpowered. Map those to `Unauthenticated` yourself, or the conformance
check for invalid secrets fails.

Use `provider.CodeOf(err)` to branch on a code and `provider.AsError(err)` to
add a hint to an error you did not create:

```go
if provider.CodeOf(err) == provider.PermissionDenied {
	return nil, provider.AsError(err).WithHint("the token needs the clusters:read scope")
}
```

Never put a secret in an error message or hint: tug prints and logs them.

### Context, timeouts and output

- **Honour the context.** Pass `ctx` to every HTTP request
  (`http.NewRequestWithContext`) and wait with `select` on `ctx.Done()`. tug
  gives `list` 30 seconds, `credential` 10 seconds, `rotate` 60 seconds and the
  rest 30 seconds; then it sends SIGTERM, and SIGKILL 2 seconds later (on
  Windows it kills the plugin at once). `provider.Main` turns SIGTERM into a
  cancelled context.
- **Never write to stdout yourself.** stdout carries exactly one JSON response,
  which `provider.Main` writes. A stray `fmt.Println` breaks the protocol and
  fails the conformance suite. Log to stderr (`fmt.Fprintln(os.Stderr, ...)` or
  a `slog` logger on `os.Stderr`); users see it with `tug --debug`.
- **Keep it quick.** tug runs `credential` whenever kubectl needs a credential
  that is not cached. Avoid work there that `list` could do.

## 5. Optional operations

Each optional operation is an interface in the SDK. Implement it, and also add
its name to `capabilities` in `plugin.yaml`: tug reads capabilities from the
manifest, so an operation the manifest does not declare is never called.
`provider.Main` adds the capability to `describe` for you.

| Operation | Interface | Capability | What tug does with it |
| --- | --- | --- | --- |
| `accounts` | `provider.AccountLister` | `accounts` | `tug import` with no arguments, and `tug import <name>` without `--account`, offer the accounts found on this machine to import. |
| `rotate` | `provider.Rotator` | `rotate` | `tug auth rotate <cluster>` calls it and stores the credential it returns in place of the cached one. |
| `checkAccess` | `provider.AccessChecker` | `checkAccess` | `tug doctor` calls it to explain an access list that blocks the caller. |

### accounts

```go
func (acmeProvider) Accounts(ctx context.Context) ([]provider.Account, error) {
	// For example, read profiles from the vendor CLI's config file.
	return []provider.Account{{ID: "work", Description: "acme CLI profile work", Config: map[string]any{"profile": "work"}}}, nil
}
```

Find accounts that are already set up on this machine, such as profiles in a
vendor CLI's config. Every account needs an `ID`. The request carries no
secrets, and the plugin runs with the scrubbed environment, so declare any
variable you read (`spec.env`). Without a terminal, `tug import` needs `--yes`
to import what it finds.

A plugin without `accounts` gets an account called `default` when the user
runs `tug import <name>` without `--account`.

### rotate

`Rotate` takes the same request as `Credential`. It should issue a new
credential, revoke the old one where the API allows, and return the new one.
If you revoke first, poll for the replacement with waits that respect `ctx`,
as the Akamai plugin does, and stay well inside the 60 seconds.

`tug auth rotate` asks before it calls `rotate`, since anything still using
the old credential loses access. If `rotate` fails, tug drops the cached
credential anyway, because the old one may already be revoked; the next
request calls `credential`. So when you revoke first and the replacement is
not ready in time, return an error whose hint says when `credential` will
work again.

### checkAccess

```go
func (acmeProvider) CheckAccess(ctx context.Context, req provider.AccessRequest) ([]provider.Finding, error) {
	// req.Cluster.ID is the ID you listed; req.EgressIP is the caller's public address, or "".
	return []provider.Finding{{
		Severity: provider.SeverityError,
		Message:  "the control plane allow list does not include 198.51.100.9",
		Fix:      "add 198.51.100.9/32 to the allow list in the acme console",
	}}, nil
}
```

`tug doctor <cluster>` runs it, before its network probes, when the cluster's
provider declares `checkAccess`. The request carries the account, your
secrets, the cluster as tug knows it (`id`, `name`, `region`, `version`,
`endpoints`, `caData`) and `egressIP`, which tug fills only when the user
passes `tug doctor --egress-ip <address>`. Return one finding per conclusion,
with severity `info`, `warning` or `error`, and a `fix` for anything the user
can act on.

The protocol also names a `serve` operation and capability, reserved for a
future long-running mode. Do not declare it: the SDK answers `Unimplemented`.

## 6. Protocol reference

This is the wire protocol, `provider/v1alpha1`, for plugins written without
the SDK and for anyone who wants to know exactly what crosses the pipe. The Go
definitions are in [`pkg/provider/protocol.go`](../pkg/provider/protocol.go)
and [`types.go`](../pkg/provider/types.go).

### Invocation

```text
tug-provider-<name> <operation>
```

- The operation is the only argument: `describe`, `accounts`, `list`,
  `credential`, `rotate` or `checkAccess` (`serve` is reserved).
- tug writes one JSON request to stdin and closes it.
- The plugin writes one JSON object to stdout, then exits: 0 for a response,
  non-zero for an `Error`. The SDK uses 1 for errors and 2 for usage errors
  such as an unknown operation.
- stderr is for logs. tug logs each line with the plugin's name and the
  operation, visible with `tug --debug`.
- The environment is scrubbed ([section 8](#8-secrets-and-the-environment)),
  and `TUG_PLUGIN=1` is set.

tug treats a non-zero exit with an `Error` on stdout as that error, and a
non-zero exit with nothing on stdout as `Internal`.

### Messages

Every message has `apiVersion: provider/v1alpha1` and a `kind`. Requests from
tug always carry both. A plugin should reject a request whose `apiVersion` or
`kind` it does not expect with `InvalidConfig`; the SDK accepts requests that
omit them. Responses must carry both, except that tug reads an `Error` whatever
its `apiVersion`.

| Operation | Request kind | Response kind |
| --- | --- | --- |
| `describe` | `DescribeRequest` | `DescribeResponse` |
| `accounts` | `AccountsRequest` | `AccountsResponse` |
| `list` | `ListRequest` | `ListResponse` |
| `credential` | `CredentialRequest` | `CredentialResponse` |
| `rotate` | `RotateRequest` | `CredentialResponse` |
| `checkAccess` | `CheckAccessRequest` | `CheckAccessResponse` |
| any | | `Error` |

Byte fields (`caData`, `certData`, `keyData`) are base64, as in a
kubeconfig's `*-data` fields. Times are RFC 3339. Omit fields you do not set.

**describe**

```json
{"apiVersion":"provider/v1alpha1","kind":"DescribeRequest"}
```

```json
{"apiVersion":"provider/v1alpha1","kind":"DescribeResponse","name":"acme","version":"0.1.0","protocols":["provider/v1alpha1"],"capabilities":["list","credential"],"secrets":[{"name":"api_token","prompt":"acme API token"}]}
```

`name` must match the manifest. `protocols` must include
`provider/v1alpha1`. `capabilities` must include `list` and `credential`;
unknown names are ignored, so newer plugins work with older hosts.

**accounts**

```json
{"apiVersion":"provider/v1alpha1","kind":"AccountsRequest"}
```

```json
{"apiVersion":"provider/v1alpha1","kind":"AccountsResponse","accounts":[{"id":"work","description":"acme CLI profile work","config":{"profile":"work"}}]}
```

**list**

```json
{"apiVersion":"provider/v1alpha1","kind":"ListRequest","account":{"id":"demo","config":{"clusters":"dev=https://127.0.0.1:6443"}},"secrets":{"api_token":"demo_test"}}
```

```json
{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[{"id":"dev","name":"dev","region":"eu-west","version":"1.33","endpoints":{"public":"https://127.0.0.1:6443"},"caData":"LS0tLS1CRUdJTi...","labels":{"env":"dev","team":"platform"}}]}
```

An empty listing is `"clusters":[]`, not `null`. A cluster may carry an inline
`credential` object in the same form as below.

**credential** and **rotate**

```json
{"apiVersion":"provider/v1alpha1","kind":"CredentialRequest","account":{"id":"demo","config":{"clusters":"dev=https://127.0.0.1:6443"}},"clusterID":"dev","secrets":{"api_token":"demo_test"}}
```

`rotate` sends the same fields with `"kind":"RotateRequest"`. Both answer:

```json
{"apiVersion":"provider/v1alpha1","kind":"CredentialResponse","credential":{"type":"token","token":"eyJhbGciOi...","expiresAt":"2026-11-20T13:00:00Z"}}
```

The four credential shapes, with the fields each one requires in bold:

| `type` | Fields |
| --- | --- |
| `token` | **`token`**, `expiresAt` |
| `clientCert` | **`certData`**, **`keyData`** (PEM, base64), `expiresAt` |
| `oidc` | **`issuer`**, **`clientID`**, `clientSecret`, `scopes` |
| `delegate` | **`exec.command`**, `exec.args`, `exec.env`, `exec.apiVersion`, `exec.interactiveMode`, `exec.installHint` |

tug rejects a credential without the fields its type requires as `Internal`.
Without `expiresAt`, the credential does not expire.

**checkAccess**

```json
{"apiVersion":"provider/v1alpha1","kind":"CheckAccessRequest","account":{"id":"work"},"cluster":{"id":"101","name":"prod-eu","endpoints":{"public":"https://101.example.com:443"}},"egressIP":"198.51.100.9","secrets":{"api_token":"..."}}
```

```json
{"apiVersion":"provider/v1alpha1","kind":"CheckAccessResponse","findings":[{"severity":"error","message":"the control plane allow list does not include 198.51.100.9","fix":"add 198.51.100.9/32 to the allow list"}]}
```

**Error**

```json
{"apiVersion":"provider/v1alpha1","kind":"Error","code":"RateLimited","message":"GET /v1/clusters: too many requests","hint":"wait a minute, then run tug sync","retryAfterSeconds":30}
```

`code` is one of the codes in [section 4](#errors); tug treats a missing code
as `Internal`. `hint` and `retryAfterSeconds` are optional.

### Limits

| Limit | Value |
| --- | --- |
| Request or response size | 16 MiB (`provider.MaxMessageBytes`). tug fails a larger response as `Internal`. |
| `list` timeout | 30 seconds |
| `credential` timeout | 10 seconds |
| `rotate` timeout | 60 seconds |
| Other operations | 30 seconds |
| After a timeout or Ctrl-C | SIGTERM, then SIGKILL after 2 seconds (Windows: killed at once) |

A timed-out call fails as `Unavailable`. Users with slow APIs can change the
timeout of any operation in tug's `config.yaml`:

```yaml
plugins:
  timeouts:
    list: 60s
    credential: 20s
    rotate: 2m
```

### Versioning

The protocol version is `provider/v1alpha1`. A plugin lists the versions it
speaks in `describe` (`protocols`) and in its manifest (`spec.protocols`); tug
refuses a manifest that lists none it speaks. Within `v1alpha1`, changes are
additive: new optional fields, operations and capabilities. Unknown
capability names are ignored, and unknown JSON fields should be ignored too,
so do not reject a request because it has a field you do not know.

## 7. Manifest reference

`plugin.yaml` is at the top of the plugin directory, and each platform's `bin`
is a path inside that directory. tug reads the manifest before it runs
anything, so listing providers starts no processes.

```yaml
apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata:
  name: acme
  version: 0.1.0
  publisher: Acme Ltd
  homepage: https://github.com/acme/tug-provider-acme
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential]
  platforms:
  - {os: darwin, arch: arm64, bin: bin/darwin_arm64/tug-provider-acme}
  - {os: linux, arch: amd64, bin: bin/linux_amd64/tug-provider-acme}
  - {os: windows, arch: amd64, bin: bin/windows_amd64/tug-provider-acme.exe}
  secrets:
  - {name: api_token, prompt: "acme API token"}
  env: [ACME_CONFIG]
  network: [api.acme.example]
  accountConfig:
    type: object
    properties:
      apiURL: {type: string, description: "API endpoint override (https)"}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `apiVersion` | yes | `tug.plugin/v1alpha1` |
| `kind` | yes | `ProviderPlugin` |
| `metadata.name` | yes | The provider name: lower-case letters, digits and dashes, up to 63 characters. Users type it in `tug import <name>`. `describe` must return the same name. |
| `metadata.version` | yes | The plugin version. Installs go to `<name>/<version>/`, and the archive is named `tug-provider-<name>_<version>.tar.gz`. Keep it equal to `Info().Version`. |
| `metadata.publisher` | no | Who publishes the plugin. Shown before install. |
| `metadata.homepage` | no | Where users can read about it. Shown before install. |
| `spec.protocols` | yes | Protocol versions the plugin speaks; must include `provider/v1alpha1`. |
| `spec.capabilities` | no | `list` and `credential`, plus any of `accounts`, `rotate`, `checkAccess`. tug assumes `list` and `credential` even when they are missing, and calls optional operations only when they are listed here. |
| `spec.platforms` | yes | One entry per OS and architecture: `os` and `arch` as in Go (`darwin`, `linux`, `windows`; `amd64`, `arm64`), and `bin`, the executable's path relative to the manifest. tug runs the entry for its own platform. |
| `spec.platforms[].sha256` | no | Leave it out: `tug plugin package` fills it in the archived manifest, and installed plugins must match it. |
| `spec.secrets` | no | Secrets tug asks for once per account and keeps in the keychain: `name`, `prompt` (what tug asks), and `optional: true` for ones the plugin can do without. The plugin receives these and no others. |
| `spec.env` | no | Host environment variables passed through to the plugin, such as `AWS_PROFILE`. Everything else is scrubbed. |
| `spec.network` | no | Hosts the plugin talks to. Shown to users before they install; tug does not enforce it. |
| `spec.accountConfig` | no | A JSON Schema for the account settings. `tug plugin describe` and `tug plugin install` list its `properties`, and `tug import` warns about `--set` keys that are not among them, unless `additionalProperties` allows others. tug does not check the values. |
| `spec.minTugVersion` | no | The oldest tug the plugin works with, such as `0.4.0`. A released tug refuses a plugin that needs a newer one when it finds, links or installs it; development builds accept every plugin. |

tug ignores fields it does not know, so that a manifest written for a newer
tug still loads. `tug plugin link`, `describe`, `install` and `package` warn
about them, so a misspelt field (`secret:` for `secrets:`) shows up as
`spec.secret`. `tug plugin package` leaves unknown fields out of the archive.

Keep the manifest and `Info()` in step: tug takes the name, version,
capabilities and secrets from the manifest, and the conformance suite checks
the name and capabilities that `describe` returns.

## 8. Secrets and the environment

**Secrets.** Declare every secret in `spec.secrets` (and in `Info().Secrets`).
tug asks for each one the first time an account is imported, with the
`prompt` you give, and keeps it in the OS keychain (or tug's encrypted file
store) under the provider, account and secret name. Every request for that
account carries them in `secrets`. tug removes any secret the manifest does not
declare before it calls the plugin, so a plugin can never read another
provider's secrets. Use lower-case names with underscores, such as
`api_token`.

**CI and scripts.** Where nobody can type a secret, set
`TUG_SECRET_<NAME>`, where `<NAME>` is the secret's name in upper case:
`TUG_SECRET_API_TOKEN` supplies `api_token`. The variable wins over the
keychain, is not stored, and applies to every account of every provider that
declares a secret of that name. On a machine without a keychain, also set
`TUG_KEYRING_BACKEND=file` and `TUG_KEYRING_PASSPHRASE`. Because the variable
name is built from the secret name, a name with a dash (`api-token`) cannot be
supplied this way from most shells; use underscores.

**The environment.** Plugins do not inherit tug's environment. They get only:

- `PATH`, `HOME`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`, `TMPDIR`, `USER`;
- `SSL_CERT_FILE`, `SSL_CERT_DIR`;
- `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` and their lower-case forms;
- on Windows, also `SYSTEMROOT`, `SYSTEMDRIVE`, `WINDIR`, `COMSPEC`,
  `PATHEXT`, `APPDATA`, `LOCALAPPDATA`, `USERPROFILE`, `TEMP`, `TMP` and
  `PROGRAMDATA`;
- the variables named in `spec.env`;
- `TUG_PLUGIN=1`.

Variables are passed only when they are set. So `TUG_SECRET_*`, cloud
credentials in the environment and anything else stay with tug unless you
declare them. Prefer secrets for anything sensitive: they live in the keychain
rather than in shell profiles.

**Network.** List the hosts you call in `spec.network`. Users see them before
they install your plugin; tug does not restrict the network.

## 9. Testing

### Unit tests with provider.Run

`provider.Run` is `provider.Main` with explicit arguments and streams. Call it
in tests to run an operation exactly as tug would, but in process. The
scaffold's `main_test.go` does this:

```go
func call(t *testing.T, op string, payload any) []byte {
	t.Helper()
	req, err := provider.EncodeRequest(op, payload)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	provider.Run(context.Background(), acmeProvider{}, []string{op}, bytes.NewReader(req), &out, &errb)
	return out.Bytes()
}

func TestList(t *testing.T) {
	out := call(t, provider.OpList, provider.ListRequest{Account: demo, Secrets: map[string]string{"api_token": "demo_x"}})
	clusters, err := provider.DecodeList(out)
	// ...
}
```

`provider.EncodeRequest` builds the request tug sends, and the `Decode*`
functions (`DecodeDescribe`, `DecodeAccounts`, `DecodeList`,
`DecodeCredential`, `DecodeCheckAccess`) read the response the way tug does:
an `Error` comes back as a `*provider.Error`, so
`provider.CodeOf(err) == provider.Unauthenticated` tests error mapping.

Test against a fake API with `net/http/httptest`, never the real one. Give
your plugin an endpoint override (an `apiURL` account setting) so tests can
point it at the fake, and cover at least: a normal listing, 401 to
`Unauthenticated`, 429 to `RateLimited` with `RetryAfterSeconds`, and a cluster
that is not ready. [`plugins/akamai/akamai_test.go`](../plugins/akamai/akamai_test.go)
and [`plugins/coreweave/coreweave_test.go`](../plugins/coreweave/coreweave_test.go)
are complete examples.

### tug plugin test

`tug plugin test <name>` runs the conformance suite against a linked or
installed plugin (or a built-in), with the same scrubbed environment tug uses.

```bash
tug plugin test acme                                      # without an account: checks that need data are skipped
TUG_SECRET_API_TOKEN=demo_test tug plugin test acme --set clusters=dev=https://127.0.0.1:6443
tug plugin test acme --account demo                       # an imported account and its stored secrets
tug plugin test acme --account demo -o json               # machine-readable report
```

`--set key=value` (repeatable) builds an account called `conformance` from
settings; with `--account`, the settings are added to the imported account's.
Secrets come from `TUG_SECRET_<NAME>` or, with `--account`, the keychain. The
command exits 1 when a check fails.

The scaffold's `make conformance` uses the demo token and settings. Once the
plugin calls a real API, change that target to test a real account, for
example `tug plugin test acme --account staging`.

### Conformance in your own tests

The suite is the public package [`pkg/conformance`](../pkg/conformance), so you
can run it from `go test` against your built binary and a fake API. Save this
as `conformance_test.go` next to `main.go` in the scaffolded project:

```go
package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	bin := filepath.Join(t.TempDir(), "tug-provider-acme")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            "acme",
		Account:         provider.Account{ID: "demo", Config: map[string]any{"clusters": "dev=https://127.0.0.1:6443"}},
		Secrets:         map[string]string{"api_token": "demo_test"},
		DeclaredSecrets: acmeProvider{}.Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the plugin does not conform")
	}
}
```

Because it builds the plugin, it skips itself under `go test -short`. With a
real API, point `Account` at your fake server (through a setting
such as `apiURL`) and pass a token the fake accepts. `conformance.Target.Env`
is the plugin's whole environment; leave it nil to inherit the test's.
`Options.Now` overrides the clock for the expiry check.

### What each check means

| Check | Passes when | Common failures |
| --- | --- | --- |
| `describe is valid` | `describe` answers a `DescribeResponse` whose `name` matches the manifest, whose `protocols` include `provider/v1alpha1`, and whose `capabilities` include `list` and `credential`. | Name in `Info()` differs from `plugin.yaml`; output before the JSON. |
| `unknown operations are rejected` | An unknown operation exits non-zero with an `Error` on stdout. | Exiting 0, or printing usage text instead of JSON. |
| `an empty list request is rejected` | `list` with empty stdin exits non-zero with an `Error`. | Treating empty input as an empty request. |
| `list returns valid clusters` | `list` succeeds, and every cluster has an `id` (unique), a `name` and an endpoint that is a URL; inline credentials are valid. Skipped when no account was given and the plugin answers `InvalidConfig`, `NotFound` or `Unauthenticated`. | A bare host name as the endpoint (add `https://`); a cluster that is not ready yet; exiting non-zero after a valid response. |
| `list is stable across calls` | A second `list` returns the same IDs. | IDs built from something that changes, such as a timestamp or a random suffix. |
| `credential is valid and unexpired` | `credential` for the first listed cluster returns a valid credential that is not already expired. Skipped when the account has no clusters. | Returning an expired token; leaving out a required field for the type. |
| `invalid secrets give Unauthenticated` | `list` with every declared secret set to `tug-conformance-invalid` fails with `Unauthenticated`. Skipped when the plugin declares no secrets. | Checking account settings before secrets, so a request without settings answers `InvalidConfig`; mapping 401 to `Internal`; an API that answers 403 for a bad token (map it to `Unauthenticated` yourself). |
| `accounts is valid` | When `describe` declares `accounts`, the operation returns accounts that each have an `id`. | |
| `SIGTERM stops the plugin promptly` | A plugin waiting for its request exits within 2 seconds of SIGTERM. Skipped on Windows. | Ignoring the context; a script that traps SIGTERM and carries on. |
| `stdout carries only one JSON object` | Every call above wrote exactly one JSON object to stdout and nothing else. | Debug output on stdout; two JSON documents. |
| `serve answers like one-shot mode` | Always skipped in this version. | |

The whole suite takes a few seconds, and skips are not failures. Run it with
real data (`--account`, or `--set` and `TUG_SECRET_*`) before you release, so
that the list and credential checks run too.

## 10. Packaging, releasing and installing

### Package

```bash
make package       # make build-all, then tug plugin package . -o dist
```

`make build-all` cross-compiles, with `CGO_ENABLED=0`, every platform in the
Makefile's `TARGETS` list, which matches the scaffold's `plugin.yaml`. If you
add or drop a platform, change both. `tug plugin package [dir] -o <dir>` (the
default output is `dist`) then writes `tug-provider-<name>_<version>.tar.gz`
containing `plugin.yaml` and the binaries, nothing else. The archived manifest
records each binary's sha256; your source manifest is not changed. The command
prints the archive's path, its platforms and the archive's own sha256:

```text
dist/tug-provider-acme_0.1.0.tar.gz
  platforms: darwin/arm64, darwin/amd64, linux/amd64, linux/arm64, windows/amd64
  sha256:    98b07c5943ae2674b04199c8f1f33cd24fb7ed56b8c03d6f0f9951dcc99b2ee9
```

Platforms whose binary is missing, or was built for another OS or
architecture, are left out with a warning, and packaging fails if none are
left. Bump `metadata.version` (and `Info().Version`) for every release.

### Release

Publish the archive and its sha256 together, for example as a GitHub release
with the sha256 in the notes. Users need both.

The scaffold's `.github/workflows/release.yml` does this when you push a `v*`
tag: it checks the tag against `metadata.version`, runs the tests, installs
tug, runs `make package` and attaches the archive to a GitHub release. While
tugcli is private, the runner needs a repository secret `TUGCLI_TOKEN` with
read access to `paingha/tugcli`, and `go.mod` must require the SDK by version.
It cannot follow a `replace` directive to a checkout on your machine, which is
what `--sdk-replace` writes. Without a token, or with a `replace`, release by
hand:

```bash
make test package
gh release create v0.1.0 dist/tug-provider-acme_0.1.0.tar.gz --notes "sha256: $(shasum -a 256 dist/tug-provider-acme_0.1.0.tar.gz | cut -d' ' -f1)"
```

The tag does not set the version: the archive name and the installed version
come from `metadata.version`, and the workflow fails when the tag is not `v`
followed by that version.

### Install

```bash
tug plugin install ./dist/tug-provider-acme_0.1.0.tar.gz --sha256 <archive sha256>
tug plugin install https://github.com/acme/tug-provider-acme/releases/download/v0.1.0/tug-provider-acme_0.1.0.tar.gz --sha256 <archive sha256>
```

`tug plugin install <file|url>` takes a `.tar.gz` or `.zip` archive with
`plugin.yaml` at the top (or inside a single top-level directory). It:

1. downloads it, over HTTPS only (plain HTTP only from a loopback address, for
   testing), when given a URL; a URL needs `--sha256`;
2. checks the archive against `--sha256` when given;
3. refuses links, special files, paths outside the plugin directory, archives
   over 512 MiB or 1,000 entries, and files over 256 MiB;
4. checks the binary for this machine's OS and architecture against its
   sha256 in the archived manifest, and refuses an archive with no build for
   this platform;
5. shows the publisher, homepage, capabilities, secrets, environment, network
   hosts and platforms, and asks you to confirm. `--yes` skips the question;
   without a terminal, `--yes` is required;
6. installs into `plugins/<name>/<version>/` under tug's data directory
   (`$TUG_HOME/data` when `TUG_HOME` is set, otherwise
   `$XDG_DATA_HOME/tug`, `~/.local/share/tug`, or `%LOCALAPPDATA%\tug` on
   Windows). Installing a version that is already there needs `--force`.

tug checks the checksums again every time it loads an installed plugin, so a
binary changed after installation is skipped, and `tug plugin ls` says why.

If the plugin is also linked, the linked copy wins: tug looks in
`$TUG_PLUGIN_PATH` first, then linked plugins, then installed ones (the
highest version of each). `TUG_PLUGIN_PATH` is a list of directories, each a
plugin directory or a directory of them, separated like `PATH`. Run
`tug plugin unlink acme` to use the installed copy. `tug plugin remove acme`
removes every installed version.

## 11. Plugins without the SDK

### Python

This needs only `python3`. The example uses the name `pyacme` so that it does
not clash with the Go walkthrough's `acme`:

```bash
tug plugin new pyacme --lang python
cd tug-provider-pyacme
python3 -m unittest
tug plugin link .
TUG_SECRET_API_TOKEN=demo_test tug plugin test pyacme --set clusters=dev=https://127.0.0.1:6443
TUG_SECRET_API_TOKEN=demo_test tug import pyacme --account lab --set clusters=pylab=https://127.0.0.1:7443
```

The scaffold writes one script, `tug-provider-pyacme`, that uses only the
standard library, plus `plugin.yaml`, `test_plugin.py` and a README. The same
script serves every platform with `python3`, so the manifest lists macOS and
Linux and `tug plugin package` needs no build step. Windows is left out
because it cannot run the script directly. Implement `list_clusters` and
`credential` with calls to your API, and raise `ProviderError(code, message,
hint)` for errors. To add an operation, add its handler to the `OPERATIONS`
table (with `True` when it reads a request) and its name to `capabilities` in
both `describe()` and `plugin.yaml`.

Package and install it as you would a Go plugin: `tug plugin package . -o
dist`, then `tug plugin unlink pyacme` and `tug plugin install` with the
sha256 it printed.

### Any language

A plugin is any executable that follows [section 6](#6-protocol-reference).
This shell plugin needs nothing but `/bin/sh`. It declares no secrets, lists
one cluster, and delegates credentials to a vendor's exec plugin:

```sh
#!/bin/sh
# tug-provider-lab: a protocol-only tug provider plugin.
V=provider/v1alpha1

fail() { # code, message, exit status
	printf '{"apiVersion":"%s","kind":"Error","code":"%s","message":"%s"}\n' "$V" "$1" "$2"
	echo "lab: $1: $2" >&2
	exit "$3"
}

read_request() {
	request=$(cat)
	[ -n "$request" ] || fail InvalidConfig "empty request" 1
}

case "${1:-}" in
describe)
	printf '{"apiVersion":"%s","kind":"DescribeResponse","name":"lab","version":"0.1.0","protocols":["%s"],"capabilities":["list","credential"]}\n' "$V" "$V"
	;;
list)
	read_request
	printf '{"apiVersion":"%s","kind":"ListResponse","clusters":[{"id":"lab-1","name":"lab","endpoints":{"public":"https://lab.example.com:6443"},"labels":{"env":"dev"}}]}\n' "$V"
	;;
credential)
	read_request
	printf '{"apiVersion":"%s","kind":"CredentialResponse","credential":{"type":"delegate","exec":{"command":"lab-cli","args":["kube-token","--cluster","lab-1"],"installHint":"install lab-cli from https://lab.example.com/cli"}}}\n' "$V"
	;;
*)
	fail InvalidConfig "unknown operation ${1:-}" 2
	;;
esac
```

Save it as `tug-provider-lab` in a new directory, make it executable, and put
this `plugin.yaml` beside it:

```yaml
apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata: {name: lab, version: 0.1.0, publisher: you}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential]
  platforms:
  - {os: darwin, arch: arm64, bin: tug-provider-lab}
  - {os: darwin, arch: amd64, bin: tug-provider-lab}
  - {os: linux, arch: amd64, bin: tug-provider-lab}
  - {os: linux, arch: arm64, bin: tug-provider-lab}
```

```bash
chmod +x tug-provider-lab
tug plugin link .
tug plugin test lab              # 8 passed, 3 skipped: it declares no secrets and no accounts
tug import lab                   # no --account and no accounts operation: the account is "default"
tug auth token --cluster lab     # runs lab-cli, or prints the install hint when it is missing
```

A real plugin parses the request (with a JSON library: `jq`, or your
language's own) to read `account`, `secrets` and `clusterID`, and escapes the
strings it prints. Everything else is the same as with the SDK.

## 12. Troubleshooting

**Is tug finding the plugin?** `tug plugin ls` lists every provider with its
version, kind (`built-in`, `plugin` or `dev plugin`), capabilities and
location, and prints any plugin it skipped, with the reason, on stderr:

```text
skipped /home/you/src/tug-provider-acme: plugin acme: stat /home/you/src/tug-provider-acme/bin/linux_amd64/tug-provider-acme: no such file or directory
```

Common reasons: the binary for this platform is not built (`make build`); the
manifest has no entry for this OS and architecture; the name is a built-in
provider's; an installed binary no longer matches its checksum
(`checksum mismatch for ...`; reinstall with `--force`). A skipped plugin is
unknown to every other command, which then reports
`unknown provider "acme" (known: [...])`.

**What does tug see?** `tug plugin describe acme` runs `describe` and prints
the manifest as tug reads it: publisher, capabilities, secrets, environment,
network hosts, platforms, account settings, the description, protocols and
the executable it will run. It warns when `describe` and the manifest
disagree on the name, and about manifest fields it does not know.

**What did the plugin say?** `tug --debug <command>` logs each plugin call
with its operation, duration and error, and every line the plugin wrote to
stderr. Plugin stderr is not shown without `--debug`.

```bash
tug --debug import acme --account demo
tug --debug auth token --cluster dev --refresh   # --refresh skips the cache and calls credential
```

**Run it by hand.** A plugin is just a program, so you can call it without
tug. `make build` writes it to `bin/<os>_<arch>/`, for example
`bin/darwin_arm64/` on an Apple silicon Mac:

```bash
echo '{"apiVersion":"provider/v1alpha1","kind":"ListRequest","account":{"id":"demo","config":{"clusters":"dev=https://127.0.0.1:6443"}},"secrets":{"api_token":"demo_x"}}' \
  | ./bin/darwin_arm64/tug-provider-acme list
```

```json
{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[{"id":"dev","name":"dev","region":"eu-west","endpoints":{"public":"https://127.0.0.1:6443"},"labels":{"env":"dev","team":"platform"}}]}
```

It runs with your full environment this way, not the scrubbed one: if it works
here but not under tug, declare the variable it needs in `spec.env`.

**Can the user reach the cluster?** `tug doctor <cluster>` checks DNS, TCP,
TLS against the CA you returned, the credential from `tug auth token`, the API
server, clock skew and RBAC, and runs your `checkAccess` when you declare it.
Use it to check the endpoint, CA and credential your plugin returns.

**Other symptoms:**

| Symptom | Likely cause |
| --- | --- |
| `acme needs api_token; set TUG_SECRET_API_TOKEN or run in a terminal` | `tug import` has no terminal to ask in. Set the variable, or run it in a terminal. |
| `Unauthenticated: no api_token stored for acme account "demo"` | The account was imported with `TUG_SECRET_API_TOKEN`, which is not stored. Run `tug import acme --account demo` in a terminal to store it, or keep the variable set. |
| `review the plugin above, then pass --yes to install it without a terminal` | `tug plugin install` has no terminal to ask in. Read what it printed, then add `--yes`. |
| `acme 0.1.0 is already installed; use --force to replace it` | Bump the version, or pass `--force` to reinstall the same one. |
| The installed plugin is not used | The plugin is also linked, and linked plugins win. Run `tug plugin unlink acme`. |
| `Unavailable: plugin acme list timed out after 30s` | The API is slow or the plugin ignores its context. Raise `plugins.timeouts.list` in `config.yaml` or make `list` faster. |
| `response is not JSON` (tug) or `extra output after the response` (`tug plugin test`) | Something besides the response went to stdout. Log to stderr. |
| `plugin returned an invalid credential` | The credential lacks a field its type needs, such as an empty token. |
| `not signed in to https://...` | The cluster uses an `oidc` credential and nobody has signed in, or the refresh token expired. Run the `tug auth login` command in the hint. |
| `<name> is a built-in provider; a plugin may replace it only if plugins.allowOverride in config.yaml lists it` | The plugin uses a built-in provider's name. Pick another name. |
| Changes to `main.go` do not show up | Rebuild (`make build` or `make link`), then `tug sync` to refresh listed data or `tug auth token --refresh` for credentials. |
| `plugin acme needs tug v0.5.0 or later and this is v0.4.0` | The plugin's `minTugVersion` is newer than tug. Upgrade tug. |
