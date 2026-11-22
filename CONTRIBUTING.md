# Contributing to tug

Thank you for helping. tug gets better with every bug report, docs fix,
provider plugin and pull request, and you don't need to be a Go or Kubernetes
expert to make a difference.

This guide covers how to report problems, set up a development copy, and get
a change merged. The code standards themselves live in
[AGENTS.md](AGENTS.md), which is written for people and AI coding agents
alike. Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

## Ways to help

- **Report a bug** or ask a question in
  [Issues](https://github.com/paingha/tugcli/issues/new/choose), with the
  form for a bug, a feature, a new provider, the docs or a question.
- **Improve the docs.** The user guide is in
  [docs/content/docs](docs/content/docs); every page has an "Open on GitHub"
  link. The plugin authors' guide is [docs/plugins.md](docs/plugins.md).
- **Write a provider plugin** for a platform tug doesn't know yet. Plugins
  live in their own repositories; see [docs/plugins.md](docs/plugins.md).
- **Fix a bug or build a feature.** For anything larger than a small fix,
  open an issue first so we can agree on the approach before you spend time
  on it.

## Reporting a bug

Include:

1. what you ran and what happened, with the full output;
2. what you expected instead;
3. `tug version`;
4. `tug doctor -o json`, if the problem is reaching a cluster.

Before you paste anything, remove cluster addresses, account IDs, user names
and anything else you'd rather not publish. tug never prints secrets, but
names and addresses can still be sensitive.

**Security problems** don't belong in public issues. Report them privately
through GitHub's **Report a vulnerability** button on the repository's
Security tab; [SECURITY.md](SECURITY.md) says what to include and what's in
scope.

## Setting up

You need:

- [Go](https://go.dev/dl/), at the version in [go.mod](go.mod) or newer;
- `make`;
- [golangci-lint](https://golangci-lint.run/) v2, for `make lint`;
- optionally [kind](https://kind.sigs.k8s.io/) and Docker, for
  `make e2e-kind`;
- optionally Node.js 20.9 or newer (22 recommended), for the docs site.

```bash
git clone https://github.com/paingha/tugcli.git
cd tugcli
make build        # bin/tug
make test         # all tests, with the race detector
```

| Task | Command |
| --- | --- |
| Build `bin/tug` | `make build` |
| All tests, with the race detector | `make test` |
| Quick tests (skip the ones that build binaries) | `make test-short` |
| One test | `go test ./internal/resolve -run TestBest` |
| Lint | `make lint` |
| Format | `make fmt` |
| Build the first-party plugins | `make plugins` |
| Python SDK tests | `make python-sdk-test` |
| End to end against a kind cluster | `make e2e-kind` |
| Run the docs site | `cd docs && npm install && npm run dev` |

Tests never touch your real keychain or home directory: they set `TUG_HOME`
to a temporary folder and use the encrypted file store. When you try a
development build by hand, do the same, so it can't change your own setup:

```bash
export TUG_HOME="$(mktemp -d)" TUG_KEYRING_BACKEND=file TUG_KEYRING_PASSPHRASE=dev
./bin/tug import kubeconfig ./some-test-kubeconfig.yaml
```

## Where things live

`cmd/tug` only calls `cli.Main`. Commands in `internal/cli` parse flags,
prompt and print, then call the package that owns the work:

| Piece | Where |
| --- | --- |
| Entry point (`os.Exit(cli.Main())` only) | [cmd/tug](cmd/tug/main.go) |
| Public plugin SDK and wire protocol | [pkg/provider](pkg/provider) |
| Cobra commands, no logic | [internal/cli](internal/cli) |
| The full-screen terminal UI behind `tug ui` | [internal/tui](internal/tui) |
| tug's directories, `config.yaml` and the editor behind `tug settings` | [internal/config](internal/config) |
| Per-terminal sessions and shell hooks | [internal/session](internal/session) |
| Cluster catalog, labels, stale marking | [internal/catalog](internal/catalog) |
| Import and sync of provider accounts | [internal/importer](internal/importer) |
| Keychain, encrypted file store, credential broker | [internal/credentials](internal/credentials) |
| Advisory file locks, so tug processes take turns | [internal/filelock](internal/filelock) |
| INI files cloud CLIs keep settings in (`~/.aws/config`, gcloud) | [internal/ini](internal/ini) |
| Provider registry | [internal/registry](internal/registry) |
| Built-in providers | [internal/builtin](internal/builtin) |
| Plugin host: discovery, manifests, runner, install, scaffolding | [internal/plugin](internal/plugin) |
| The OS sandbox plugins run in | [internal/sandbox](internal/sandbox) |
| Sigstore signature checks on plugin archives | [internal/sigstore](internal/sigstore) |
| Protocol conformance suite (public) | [pkg/conformance](pkg/conformance) |
| Client factory and discovery cache | [internal/kube](internal/kube) |
| Read-only guard as an `http.RoundTripper` | [internal/guard](internal/guard) |
| The audit log behind `tug audit` | [internal/audit](internal/audit) |
| kubectl passthrough | [internal/passthrough](internal/passthrough) |
| Running one command on many clusters (`--cluster-selector`) | [internal/fanout](internal/fanout) |
| Fuzzy target and workload resolution | [internal/resolve](internal/resolve) |
| API discovery, CRDs and schemas, upstream API removals and support windows | [internal/apiinfo](internal/apiinfo) |
| Diagnostics behind `tug doctor` | [internal/doctor](internal/doctor) |
| Policy packs behind `tug check` | [internal/policy](internal/policy) |
| tugd: local API proxy, named port-forwards, control socket | [internal/daemon](internal/daemon) |
| Tunnels: SSH, proxies, AWS SSM, Google IAP, Teleport's tsh | [internal/tunnel](internal/tunnel) |
| OpenID Connect sign-in | [internal/oidc](internal/oidc) |
| Opening the web browser for sign-in | [internal/browser](internal/browser) |
| `.tug.yaml` pins and their trust | [internal/pin](internal/pin) |
| Team catalogs: signing, fetching, merging, policies | [internal/team](internal/team) |
| Working-environment profiles | [internal/profile](internal/profile) |
| Bookmarks and command aliases | [internal/bookmark](internal/bookmark), [internal/alias](internal/alias) |
| Finding the file that defines an object, and opening an editor | [internal/manifest](internal/manifest), [internal/editor](internal/editor) |
| Logs, exec, debug containers, port-forward, rollouts, why, tree and reconcile (shared by CLI and TUI) | [internal/ops](internal/ops) |
| Diffs, server-side dry runs, field ownership and drift across clusters | [internal/diff](internal/diff) |
| Support bundles: collection, redaction and the archive | [internal/bundle](internal/bundle) |
| Model providers behind `tug why --deep`: evidence, redaction, prompts, prices, usage | [internal/intelligence](internal/intelligence) |
| Flux and Argo CD status, explain, drift and dependencies | [internal/gitops](internal/gitops) |
| Tables, JSON/YAML, colour | [internal/render](internal/render) |
| The build's version, set at link time | [internal/version](internal/version) |
| First-party plugins | [plugins](plugins) |
| Python SDK for plugins | [sdk/python](sdk/python) |
| Example plugin | [examples/tug-provider-example](examples/tug-provider-example) |
| User guide (the docs site) | [docs/content/docs](docs/content/docs) |
| Plugin authors' guide and protocol reference | [docs/plugins.md](docs/plugins.md) |

## Making a change

1. **Branch** from `main`.
2. **Follow [AGENTS.md](AGENTS.md).** The rules people most often miss:
   - `internal/cli` only parses, prompts and renders; the work happens in the
     other `internal/` packages, which never print or prompt.
   - Wrap errors with what was being done
     (`fmt.Errorf("listing clusters: %w", err)`), and make errors a person
     reads say what to do next.
   - Messages, help text and docs use British spelling (colour, normalise);
     Go identifiers use American spelling, as Go does.
   - A secret never goes in a file, a log, an error or a test fixture.
   - `pkg/provider` uses the standard library only, and `plugins/` never
     import `internal/`.
3. **Test it.** Use the standard library's `testing` package, table-driven
   where there are several cases, and run commands end to end through
   `cli.Run` with the helpers in
   [internal/cli/cli_test.go](internal/cli/cli_test.go). A bug fix comes with
   a test that fails without it.
4. **Check it** before you push:

   ```bash
   make lint test
   go mod tidy      # CI fails if this changes go.mod or go.sum
   ```

   CI runs the tests on Linux, macOS and Windows, so platform-specific code
   (keychain, sessions, process handling) is checked on all three.
5. **Update the docs** when a command, flag or output changes: the README
   where it shows them, and the pages under
   [docs/content/docs](docs/content/docs). Sample output in the docs is copied
   from real runs, so paste the new output rather than editing by hand.

## Commits and pull requests

- Keep each commit to one purpose. Write the subject in the imperative,
  under about 72 characters, with no trailing full stop:
  `Add tug auth rotate for providers that can rotate`.
- In the body, say what changed and why, wrapped at 72 columns. For larger
  changes, add a bullet per package.
- Don't add AI attribution to commits, pull requests or files: no
  `Co-Authored-By` trailer for an assistant and no "Generated with" line.
- Never commit build output (`bin/`, `dist/`, `plugins/*/bin/`,
  `docs/out/`) or anything that contains a secret.
- Add a new Go module only when the standard library or an existing
  dependency can't do the job, and say why in the commit message.
- In the pull request, explain the problem, the fix, and how you tested it.
  The pull request template asks for these, with a short checklist of what
  reviewers look for first.

A maintainer reviews every pull request. Small, focused changes are reviewed
fastest.

## Changing a first-party plugin

The plugins in [plugins/](plugins) are released with tug: each release
attaches `tug-provider-<name>_<version>.tar.gz`, named after the version in
the plugin's `plugin.yaml`. When you change a plugin, bump that version, so
people who installed the old one can tell them apart. `make plugins-dist`
builds the release archives locally, in `dist/plugins`.

## Changing the plugin protocol

The protocol in [pkg/provider](pkg/provider) is a public contract. Changes must
stay compatible with `provider/v1alpha1`, and must update
[pkg/conformance](pkg/conformance), the example in
[examples/tug-provider-example](examples/tug-provider-example) and
[docs/plugins.md](docs/plugins.md) in the same pull request.

## Licence

tug is released under the [MIT License](LICENSE). By contributing, you agree
that your contributions are licensed under it too.
