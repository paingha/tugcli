<!--
Thanks for the pull request. AGENTS.md has the standards this repository
follows; the checklist below is the part reviewers look for first. Delete any
line that doesn't apply.
-->

## What and why

<!-- What this changes, and the problem it solves. Link the issue it fixes, e.g. "Fixes #123". -->

## How

<!-- For larger changes, a line per package, as in the commit bodies. -->

## Testing

<!-- What you ran, and on which OS. Say if lint was not run, or if anything was only tried by hand. -->

- [ ] `make lint test` passes (or: lint wasn't run, because …)
- [ ] `go mod tidy` changes nothing
- [ ] A bug fix has a test that fails without it
- [ ] Platform-specific code (keychain, sessions, process handling) was tested on the OS it targets
- [ ] Tested against a real cluster: <!-- which kind, or "not tried" -->

## Checklist

- [ ] Commits are small, each with one purpose, and their subjects are imperative
- [ ] The README and the pages under `docs/content/docs` show any command, flag or output that changed
- [ ] JSON and YAML output only gained fields; nothing was renamed or removed
- [ ] No secret is written to a file, a log, an error message or a test fixture, or taken from a flag
- [ ] Writes on read-only sessions still go through the guard
- [ ] Protocol changes stay compatible with `provider/v1alpha1` and update `pkg/conformance` and `examples/tug-provider-example`
- [ ] New modules are justified in the commit message
- [ ] No AI attribution lines in commits or this description
