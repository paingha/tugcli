# Copilot instructions

The full standards for this repository are in [AGENTS.md](../AGENTS.md) at the
repository root. Read it before writing or reviewing code. If anything here
differs from AGENTS.md, AGENTS.md is right.

The rules that matter most, for tools that read only this file:

- tug is a Go CLI built with cobra. Check changes with `make lint test`.
- `internal/cli` only parses flags, prompts and renders; logic lives in the
  other `internal/` packages, which never print or prompt.
- `pkg/provider` is public and standard library only. `plugins/` never import
  `github.com/paingha/tugcli/internal/...`.
- Wrap errors with `%w` and the action in progress (`listing clusters: %w`);
  check them with `errors.Is` and `errors.As`. Errors a person reads say what
  to do next.
- stdout carries results only; prompts, warnings and logs go to stderr.
  Commands write to `a.stdout` and `a.stderr`, never `os.Stdout`.
- Every command works without a terminal, and every prompt has a flag that
  answers it (`--yes`). Secrets never come from flags.
- Never write a secret to disk outside the credential store, or to a log or
  error message.
- Tests use the standard library, `httptest` fakes and `isolate(t)` so they
  never touch the network, the real keychain or the home directory. They pass
  on Linux, macOS and Windows.
- Do not add AI attribution to commits, pull requests or files.
