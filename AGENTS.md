# AGENTS.md

How to work on tug. This file is for AI coding agents (Codex, Claude Code,
GitHub Copilot, Cursor and others) and for people. It is the single source of
truth: `CLAUDE.md` and `.github/copilot-instructions.md` point here, so change
the standards in this file only.

tug is a Go CLI that makes Kubernetes clusters easy to reach: a cluster catalog
fed by providers, per-terminal sessions, credentials in the OS keychain, and a
JSON-over-stdio plugin protocol. [README.md](README.md) says what it does and
which package holds each piece.

## Commands

| Task | Command |
| --- | --- |
| Build `bin/tug` | `make build` |
| All tests, with the race detector | `make test` |
| Quick tests (skip ones that build binaries) | `make test-short` |
| One test | `go test ./internal/resolve -run TestBest` |
| Lint (golangci-lint v2, [.golangci.yml](.golangci.yml)) | `make lint` |
| Format | `make fmt` |
| Build the first-party plugins | `make plugins` |
| End to end against a kind cluster | `make e2e-kind` |

Before calling a change done, run `make lint test`. If golangci-lint is not
installed, run `go vet ./...` and `make test`, and say that lint was not run.
After changing imports or go.mod, run `go mod tidy`; CI fails when it would
change anything. CI runs the tests on Linux, macOS and Windows, so
platform-specific code (keychain, sessions, process handling) is checked on
all three.

## Layout and boundaries

```text
cmd/tug/           main only: os.Exit(cli.Main())
internal/cli/      cobra commands: flags, prompts, output; no business logic
internal/<area>/   the logic, one package per concern (see the README table)
internal/ops/      logs, exec, port-forward, rollouts; no presentation
pkg/provider/      public plugin SDK and wire protocol; standard library only
pkg/conformance/   public protocol conformance suite
plugins/<name>/    first-party plugins, built only on pkg/
examples/          example plugin
```

- `cmd/tug/main.go` stays one line. Everything starts at `cli.Main`, which
  calls `cli.Run(ctx, args, stdin, stdout, stderr)`.
- `internal/cli` parses, prompts and renders, then calls the package that owns
  the work. If no package fits, add one under `internal/` with a package
  comment rather than growing `internal/cli`.
- Packages outside `internal/cli` never print, prompt or colour. They return
  data, take callbacks (`ops.LogSink`) or log through a `*slog.Logger` they
  are given. `internal/ops` is shared with a future TUI, so it must stay free
  of presentation.
- `pkg/` is public API for plugin authors. `pkg/provider` imports the
  standard library only. Protocol changes stay compatible with
  `provider/v1alpha1` and update `pkg/conformance` and
  `examples/tug-provider-example` in the same change.
- `plugins/` import `pkg/...` and `plugins/internal/...`, never
  `github.com/paingha/tugcli/internal/...`; they are meant to move to their
  own repositories.
- Add a module only when the standard library or an existing dependency
  cannot do the job, and say why in the commit message.

## Go standards

The baseline is [Effective Go](https://go.dev/doc/effective_go),
[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) and the
[Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md).
The linters enforce much of that. The rules below are the ones this repository
has chosen or that linters cannot check.

### Formatting, naming and comments

- gofmt and goimports, with three import groups: standard library, third
  party, then `github.com/paingha/tugcli/...`. Alias imports only for clashes
  and Kubernetes conventions (`corev1`, `clientcmdapi`).
- Every package has a package comment, and every exported identifier has a
  doc comment that starts with its name. Comments are full sentences that say
  what something is for or why it is done this way, not what the next line
  does.
- Identifiers use American spelling, as Go does (`Colors`). Comments, help
  text, messages and docs use British spelling (colour, normalise, honour),
  as the existing code does.
- Initialisms keep one case (`ID`, `URL`, `HTTP`, `clusterID`). Getters have
  no `Get` prefix. Receivers are short and the same for every method of a
  type.

### Errors

- Return errors; do not panic outside programmer errors during
  initialisation.
- Wrap with `%w` and the action that was in progress:
  `fmt.Errorf("listing workloads in %s: %w", ns, err)`. Error strings are
  lowercase, have no trailing punctuation, and do not say "failed to" or
  "error".
- Check errors with `errors.Is` and `errors.As`, never `==` on wrapped errors
  or matching on `err.Error()`.
- Sentinel errors are `var ErrX = errors.New("...")` in the package that
  returns them (`catalog.ErrNotFound`, `session.ErrNoSession`).
- An error a person will read says what to do next:
  ``not connected; run `tug connect` first``. Provider errors are
  `*provider.Error` with a `Code`, plus a `Hint` when there is an obvious
  fix; the CLI prints the hint on its own line.
- Handle every error. Discard one only with `_ =` where failure is harmless,
  as in `_ = cmd.MarkFlagRequired("cluster")`. A `//nolint` names the linter
  and gives a reason: `//nolint:errcheck // best effort on the way out`.

### Design

- Anything that does I/O or can block takes `ctx context.Context` as its
  first parameter and stops when it is cancelled. Do not store contexts in
  structs.
- Inject what tests need to control instead of reading globals deep in the
  code: `now func() time.Time`, `getenv func(string) string`, `io.Reader` and
  `io.Writer`. Constructors with optional settings take functional options:
  `kubeconfig.New(kubeconfig.WithHome(dir))`.
- Assert interface implementations at compile time:
  `var _ registry.Provider = (*Provider)(nil)`.
- Keep interfaces small and declare them in the package that uses them.
- Use the standard library first: `slices`, `maps`, `errors.Join`,
  `log/slog`, `min`/`max`, range over integers. The module targets the Go
  version in go.mod.
- Log with `log/slog` to stderr through a logger that is passed in. tug logs
  at Warn by default and Debug with `--debug`.
- Every goroutine has an owner and a way to stop (a context or a closed
  channel). Tests run with `-race`.
- Embed non-Go assets with `//go:embed`, as the shell hooks and plugin
  templates are.

### Secrets, files and platforms

- A secret lives only in the credential store. Never write one to a
  kubeconfig, the catalog, a log, an error message or a test fixture. Session
  kubeconfigs authenticate through `tug auth token`.
- Write state files with `config.WriteFileAtomic`. Files under tug's
  directories are 0o600 and directories 0o700; 0o644 and 0o755 are only for
  plugin binaries and scaffolded projects.
- Find tug's directories through `config.Paths`, which honours `TUG_HOME` and
  the XDG variables. Never hard-code `~/.config`, `~/.kube` or `/tmp`.
- Use `path/filepath` for file paths. Platform-specific code goes in a pair of
  files, `x_unix.go` with `//go:build !windows` and `x_windows.go` with
  `//go:build windows`, defining the same functions. Windows has no
  `syscall.Exec` and no Unix permission bits.
- Plugins run with a scrubbed environment and get only the secrets their
  manifest declares. Do not widen that.

## CLI standards

### Commands

- Each command has a constructor `newXxxCmd(a *app) *cobra.Command` in the
  `internal/cli/cmd_<area>.go` file for its area, and is registered in
  `root.go` under a help group: connect, interact, discover, safety or other.
  Plumbing that only tug or its shell hooks call is `Hidden: true`.
- Runnable commands set `Args` (`cobra.NoArgs`, `cobra.ExactArgs(1)`, or
  `cobra.ArbitraryArgs` when any number is valid) and use `RunE`, never
  `Run`. Get the context from `cmd.Context()`.
- A command with several flags keeps them in an unexported `xxxOptions`
  struct.
- `Use` writes arguments as `<required>` and `[optional]`. `Short` starts
  with a capital letter and has no trailing period. `Example` lines are
  indented two spaces and end with a `# comment` when the effect is not
  obvious. Add `Long` only when there is more to say than `Short`.
- tug runs any command it does not have as kubectl. A new top-level command or
  alias hides the kubectl command of the same name, so choose names that do
  not collide unless replacing kubectl's command is the point.
- When a command, flag or output changes, update the README where it shows
  them.

### Flags

- Long names are lowercase kebab-case. Short flags are for the common ones,
  with kubectl's meanings: `-n/--namespace`, `-o/--output`, `-c/--container`,
  `-l/--selector`, `-f/--follow`, `-y/--yes`. Never give a short letter a
  second meaning.
- Help text is lowercase with no trailing period, and states the default when
  it is not obvious: `"namespace (default: the session's)"`.
- Output format is `-o/--output`, parsed by `render.ParseFormat` (table, json,
  yaml, name). Write tables with `render.Table` and JSON or YAML with
  `render.Structured`.
- Repeatable settings are `StringArrayVar` flags such as `--set key=value`,
  not comma-separated values.
- Secrets never come from flags or arguments, which end up in shell history
  and process lists. Ask with `a.readSecret`, or read `TUG_SECRET_<NAME>` in
  scripts and CI.

### Output and exit codes

- stdout carries the result only, which is what a script pipes or parses.
  Prompts, progress, warnings, hints and logs go to stderr.
- Commands write to `a.stdout` and `a.stderr`, never `os.Stdout`,
  `os.Stderr` or `fmt.Print*`, so tests can run `cli.Run` against buffers.
- JSON and YAML output are a stable interface: add fields, do not rename or
  remove them.
- Colour goes through `render.Colors` from `a.colors()`. It is off when the
  stream is not a terminal, `NO_COLOR` is set or `TERM=dumb`, and never
  appears in JSON or YAML.
- Return errors up to `cli.Run`, which prints `tug: <error>` (and
  `hint: ...`) and exits 1. Do not print an error and also return it. To exit
  with another program's code (kubectl, a command run in a pod) without
  printing more, return `&exitError{code: n}`.
- Ctrl-C cancels the command's context. Streaming commands treat
  `context.Canceled` as a normal end.

### Prompts and safety

- Prompt only through the app's helpers: `a.confirm`, `a.pick`,
  `a.pickMany` and `a.readSecret`. They ask on stderr, and only when stdin and
  stderr are terminals.
- Every command works without a terminal. There, `confirm` returns its
  default, `pick` fails with a "be more specific" message, and input that is
  required returns `errNotInteractive`. Give scripts a flag for each question
  (`--yes`, `--account`).
- Writes on a read-only session (`env=prod`) go through `a.ensureWritable`,
  which asks once before the first request and refuses without a terminal.
  Never add a path around the guard.
- Commands that grant trust, such as `plugin install`, show what will happen
  (publisher, secrets, network hosts) and ask first; `--yes` skips the
  question. Commands that remove things say what they removed.

## Testing

- Standard library `testing` only, without assertion libraries. Failures read
  `Best("prod") = [a], want [b]`: call, got, want.
- Table-driven tests with `t.Run` for anything with more than a couple of
  cases.
- Test commands end to end through `cli.Run` with the helpers in
  [internal/cli/cli_test.go](internal/cli/cli_test.go) (`tug`, `mustTug`,
  `isolate`). Kubernetes and provider APIs are `httptest` fakes; tests never
  reach the network or a real cluster.
- Tests never touch the real keychain or home directory: call `isolate(t)`,
  or set `TUG_HOME` to `t.TempDir()` with `TUG_KEYRING_BACKEND=file` and
  `TUG_KEYRING_PASSPHRASE`.
- Use `t.TempDir`, `t.Setenv`, `t.Cleanup` and `t.Helper`. Tests that call
  `t.Setenv`, including through `isolate`, cannot use `t.Parallel`.
- Tests that build binaries or are slow skip when `testing.Short()` is true.
- Tests pass on Linux, macOS and Windows: build paths with `filepath.Join`,
  and guard Unix-only checks such as file modes with `runtime.GOOS`.
- A bug fix comes with a test that fails without it. Provider and plugin
  changes keep passing `pkg/conformance`.

## Commits and pull requests

- Small commits with one purpose. The subject is imperative, under about 72
  characters, with no trailing period, such as
  `Add AWS EKS and Google GKE built-in providers`. The body says what changed
  and why, wrapped at 72 columns, with a bullet per package for larger
  changes.
- Do not add AI attribution to commits, pull requests or files: no
  `Co-Authored-By` trailer for an assistant and no "Generated with" line.
- Never commit build output (`bin/`, `dist/`, `plugins/*/bin/`) or anything
  containing a secret.
