# Security policy

## Reporting a vulnerability

Please do not report security problems in a public issue, pull request or
discussion.

Report them privately through GitHub instead: open the repository's
**Security** tab and choose **Report a vulnerability**, or go straight to
[the advisory form](https://github.com/paingha/tugcli/security/advisories/new).
Include:

- the tug version (`tug version`) and your operating system;
- what you found, and its impact as you see it;
- the steps, configuration or proof of concept that reproduce it;
- whether you have shared it with anyone else.

You will get an acknowledgement, and we will keep you told of progress while
we work out a fix. Once a fix is released, we publish an advisory and credit
you, unless you would rather not be named. Please give us a reasonable chance
to release a fix before you disclose anything publicly.

## Supported versions

tug is in early development. Fixes go into the latest release only, so please
check that a problem still happens there before you report it.

## Scope

In scope:

- the `tug` CLI and the tugd local API proxy;
- how tug stores and handles credentials;
- the read-only guard, the plugin sandbox, and plugin, index and team catalog
  verification;
- the first-party plugins in [plugins](plugins), the Go plugin SDK in
  [pkg/provider](pkg/provider) and the Python SDK in [sdk/python](sdk/python);
- tug's release artefacts and their signatures.

Out of scope, and better reported elsewhere:

- third-party plugins: report those to their publishers;
- vulnerabilities in Kubernetes, kubectl, cloud providers or identity
  providers;
- ways around the read-only guard by someone who means to get around it,
  such as using another kubeconfig or running `tug unlock`. The guard stops
  honest mistakes and is not a security boundary; see
  [What tugd protects, and what it doesn't](https://paingha.github.io/tugcli/docs/concepts/proxy-limits).
  A way for a request tug treats as a read to change a cluster *is* in scope.
- attacks that need control of your user account or machine.

## How tug approaches security

The [security guide](https://paingha.github.io/tugcli/docs/concepts/security)
explains what tug trusts, how it handles credentials and plugins, and where
its protection stops.
