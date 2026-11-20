# tug-provider

The Python SDK for [tug](https://github.com/paingha/tugcli) provider plugins.
It speaks protocol `provider/v1alpha1`, mirrors the Go SDK,
[`pkg/provider`](https://github.com/paingha/tugcli/tree/main/pkg/provider),
and writes the same JSON. It needs Python 3.9 or later and nothing outside
the standard library.

The [plugin guide](https://github.com/paingha/tugcli/blob/main/docs/plugins.md)
explains what tug expects from a provider; this README shows how to do it in
Python.

## Install

The package is to be published on PyPI as `tug-provider`; the import name is
`tug_provider`. Until then, install it from a checkout of tugcli:

```bash
python3 -m pip install ./sdk/python
```

## A plugin

A plugin subclasses `Provider`, implements `info`, `list` and `credential`,
and hands an instance to `main`:

```python
#!/usr/bin/env python3
"""tug-provider-acme: Acme Kubernetes clusters for tug."""

import json
import sys
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone

from tug_provider import (
    UNAUTHENTICATED,
    UNAVAILABLE,
    Cluster,
    Endpoints,
    Info,
    Provider,
    ProviderError,
    SecretSpec,
    main,
    status_error,
    token,
)


class AcmeProvider(Provider):
    def info(self):
        return Info(
            name="acme",
            version="0.1.0",
            description="Acme Kubernetes Engine",
            secrets=[SecretSpec("api_token", prompt="acme API token")],
        )

    def list(self, request):
        return [
            Cluster(
                id=c["id"],  # stable for the life of the cluster
                name=c["name"],
                region=c["region"],
                endpoints=Endpoints(public=c["endpoint"]),
                labels={"env": c["environment"]},
            )
            for c in self.get(request, "/v1/clusters")
            if c["endpoint"]  # skip clusters that are not ready yet
        ]

    def credential(self, request):
        t = self.get(request, "/v1/clusters/%s/token" % request.cluster_id)
        return token(t["token"], datetime.now(timezone.utc) + timedelta(seconds=t["ttl"]))

    def get(self, request, path):
        api_token = request.secrets.get("api_token")
        if not api_token:
            raise ProviderError(UNAUTHENTICATED, "no api_token", hint="run tug import acme again to set one")
        base = request.account.config_string("apiURL") or "https://api.acme.example"
        req = urllib.request.Request(base + path, headers={"Authorization": "Bearer " + api_token})
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                return json.load(resp)
        except urllib.error.HTTPError as e:
            raise status_error(e.code, "GET %s: %s" % (path, e.reason), e.headers) from e
        except urllib.error.URLError as e:
            raise ProviderError(UNAVAILABLE, "GET %s: %s" % (path, e.reason)) from e


if __name__ == "__main__":
    sys.exit(main(AcmeProvider()))
```

Save it as `tug-provider-acme`, make it executable, and write a `plugin.yaml`
beside it whose platforms all point at the script, as the
[example's](https://github.com/paingha/tugcli/blob/main/sdk/python/examples/tug-provider-pyexample/plugin.yaml)
do. Then:

```bash
tug plugin link .
TUG_SECRET_API_TOKEN=... tug plugin test acme --account staging
```

`main` reads the operation from `sys.argv` and one request from stdin, calls
your method, writes exactly one JSON response to stdout, and returns the exit
code: 0 for a response, 1 for an error, 2 for a usage error such as an
unknown operation. It refuses a request for another protocol version or
operation with `InvalidConfig`, accepts requests that leave out `apiVersion`
and `kind`, and rejects an empty request for the operations that need one.
With the operation `serve`, `main` answers many requests in one process
instead; see [Serve mode](#serve-mode).

## The API

The names follow the Go SDK, in Python's spelling. Fields are snake_case
(`cluster_id`, `ca_data`, `expires_at`); on the wire they are the protocol's
camelCase.

| Go SDK | Python SDK |
| --- | --- |
| `provider.Provider` | `Provider`, with `info()`, `list(request)` and `credential(request)` |
| `AccountLister`, `Rotator`, `AccessChecker` | override `accounts()`, `rotate(request)` or `check_access(request)` |
| `provider.Capabilities(p)` | `capabilities(p)`, a list of names |
| `provider.Main(p)`, `provider.Run(...)` | `main(p)`, `run(p, args, stdin, stdout, stderr)`, which also serve |
| `ctx.Done()`, `ctx.Err()` | `sleep(seconds)`, `canceled()` and the `Canceled` exception |
| `Info`, `SecretSpec`, `Account`, `Cluster`, `Endpoints`, `ExecConfig`, `Credential`, `Finding`, `Description` | dataclasses of the same names |
| `ListRequest`, `CredentialRequest`, `AccessRequest` | dataclasses of the same names |
| `Token`, `ClientCert`, `OIDC`, `Delegate` | `token`, `client_cert`, `oidc`, `delegate` |
| `NoExpiry` | `NO_EXPIRY`, which is `None` |
| `Credential.Validate`, `Credential.Expired` | `Credential.validate()`, which raises `ValueError`, and `Credential.expired()` |
| `Account.ConfigString` | `Account.config_string` |
| `*provider.Error`, `Errorf`, `WithHint` | `ProviderError(code, message, hint=..., retry_after_seconds=...)`, `with_hint` |
| `Unauthenticated`, `RateLimited`, ... | `UNAUTHENTICATED`, `RATE_LIMITED`, ... |
| `CodeOf`, `AsError`, `StatusError` | `code_of`, `as_error`, `status_error(status, message, headers)` |
| `EncodeRequest`, `DecodeList`, ... | `encode_request`, `decode_list`, ... |
| `OpList`, `CapRotate`, `SeverityError`, `ExitError`, ... | `OP_LIST`, `CAP_ROTATE`, `SEVERITY_ERROR`, `EXIT_ERROR`, ... |

Each type has `to_dict()`, which returns the JSON object the Go SDK writes,
and `from_dict()`, which reads one as the Go SDK does.

### Optional operations

Override `accounts`, `rotate` or `check_access` to support it, and `describe`
lists its capability. Also add the capability to `plugin.yaml`: tug reads
capabilities from the manifest, so it never calls an operation the manifest
does not declare. An operation you do not override answers `Unimplemented`.

```python
class AcmeProvider(Provider):
    ...

    def accounts(self):
        # For example, read profiles from the vendor CLI's config file.
        return [Account("work", description="acme CLI profile work", config={"profile": "work"})]

    def check_access(self, request):
        # request.cluster is the cluster as tug knows it; request.egress_ip
        # is the caller's public address, or "".
        return [
            Finding(
                SEVERITY_ERROR,
                "the control plane allow list does not include " + request.egress_ip,
                fix="add %s/32 to the allow list in the acme console" % request.egress_ip,
            )
        ]
```

### Credentials

`token(token, expires_at)`, `client_cert(cert_pem, key_pem, expires_at)`,
`oidc(issuer, client_id, *scopes, client_secret="")` and
`delegate(command, *args, env=None, install_hint="", ...)` build the four
shapes. Give anything short-lived its expiry as an aware `datetime`, which is
converted to UTC; pass `NO_EXPIRY` only for credentials that really do not
expire. A naive `datetime` is refused, because it could be in any time zone.
PEM may be `bytes` or `str`.

### Errors

Raise `ProviderError` with one of the codes; tug prints the code and message,
and the hint on a line of its own. Any other exception reaches tug as
`Internal`, with its traceback in tug's log (`tug --debug`). For HTTP APIs,
`status_error(status, message, headers)` maps 401 to `Unauthenticated`, 403
to `PermissionDenied`, 404 to `NotFound`, 429 to `RateLimited` (with the
`Retry-After` seconds), 408 and 5xx to `Unavailable`, 400 and 422 to
`InvalidConfig`, and anything else to `Internal`. `headers` may be urllib's
`HTTPMessage` or any mapping. Never put a secret in a message or hint: tug
prints and logs them.

`code_of(err)` and `as_error(err)` follow `raise ... from` chains, as Go's
`errors.As` follows wrapped errors:

```python
try:
    clusters = self.get(request, "/v1/clusters")
except Exception as e:
    if code_of(e) == PERMISSION_DENIED:
        raise as_error(e).with_hint("the token needs the clusters:read scope") from e
    raise
```

### Timeouts, cancellation and output

tug gives `list` 30 seconds, `credential` 10 and `rotate` 60. In one-shot
mode it then sends SIGTERM, and `main` turns SIGTERM and SIGINT into a
`Canceled` exception, raised in the main thread wherever the provider is.
The SDK answers `Unavailable` and exits 1, so `finally` blocks and `with`
statements run on the way out. `Canceled` derives from `BaseException`,
like `KeyboardInterrupt`, so `except Exception` does not swallow it. Still
set a timeout on every network call: a call blocked inside C code may not
see the signal until it returns.

In serve mode a request is cancelled without a signal, and the SDK does not
interrupt the thread answering it. Instead `canceled()` turns true and
`sleep(seconds)`, a `time.sleep` that wakes when the request is cancelled,
raises `Canceled`. So wait with `sleep`, and check `canceled()` between the
steps of long work; both work in one-shot mode too.

```python
from tug_provider import NO_EXPIRY, UNAVAILABLE, ProviderError, sleep, token


class AcmeProvider(Provider):
    ...

    def rotate(self, request):
        old = self.get(request, "/v1/clusters/%s/token" % request.cluster_id)
        self.post(request, "/v1/tokens/%s/revoke" % old["id"])
        for _ in range(20):
            new = self.get(request, "/v1/clusters/%s/token" % request.cluster_id)
            if new["id"] != old["id"]:
                return token(new["token"], NO_EXPIRY)
            sleep(2)  # raises Canceled when tug gives up on the request
        raise ProviderError(UNAVAILABLE, "no new token yet", hint="run tug auth token again in a minute")
```

stdout carries exactly one JSON response, or in serve mode one per line. While the provider runs, `main`
points `sys.stdout` at stderr, so a stray `print()` ends up in tug's log
rather than breaking the protocol. Log with `logging` or
`print(..., file=sys.stderr)`; users see it with `tug --debug`. Child
processes still inherit the real stdout, so capture their output.

## Serve mode

Every plugin built with the SDK can serve: tug starts it once as
`tug-provider-acme serve` and sends many requests, which saves a process
start per call. tug does so only when `plugin.yaml` lists `serve` in its
capabilities, so add it there; `describe` always lists it. There is nothing
else to implement.

In serve mode each line of stdin is one frame, `{"id", "op", "request"}`,
and the SDK answers each request in a thread of its own with one line,
`{"id", "response"}`, whose response is exactly what one-shot mode would
write. Answers may come out of order, and lines never interleave. A frame
`{"id", "cancel": true}` cancels that request. An unknown operation gets an
`InvalidConfig` error with the request's id, and a line that is not JSON or
has no id gets one with an empty id; serving carries on. When stdin closes,
the plugin finishes the requests in flight and exits 0. On SIGTERM or SIGINT
it cancels them all, gives them a second to answer, and exits 0 without the
rest.

Because requests run concurrently, a provider's methods must be safe to run
at the same time: keep per-request state in local variables, and guard
anything shared, such as a cache, with a lock.

## Testing

`run` is `main` with explicit arguments and streams, and `encode_request`
and the `decode_*` functions build and read messages as tug does, so a test
calls an operation exactly as tug would, in process:

```python
import io
import unittest

from tug_provider import OP_LIST, UNAUTHENTICATED, Account, ListRequest, ProviderError, decode_list, encode_request, run


def call(op, payload=None):
    out, err = io.BytesIO(), io.StringIO()
    run(AcmeProvider(), [op], io.BytesIO(encode_request(op, payload)), out, err)
    return out.getvalue()


class ListTest(unittest.TestCase):
    def test_no_token(self):
        out = call(OP_LIST, ListRequest(account=Account("demo")))
        with self.assertRaises(ProviderError) as cm:
            decode_list(out)
        self.assertEqual(cm.exception.code, UNAUTHENTICATED)
```

Point the plugin at a fake API (`http.server` on a loopback port, through an
`apiURL` account setting), never the real one. Then run the conformance suite
with `tug plugin test`, as the plugin guide's
[testing section](https://github.com/paingha/tugcli/blob/main/docs/plugins.md#9-testing)
describes.

## Shipping a plugin

`tug plugin package` archives `plugin.yaml` and the files its platforms name,
nothing else, so an installed plugin cannot import an SDK that sat beside it.
Either ask users to install `tug-provider` for the `python3` on their `PATH`,
or bundle the SDK and your script into one executable file with the standard
library's `zipapp`:

```bash
mkdir -p build/app bin
cp tug-provider-acme build/app/__main__.py
python3 -m pip install --target build/app tug-provider   # or copy sdk/python/tug_provider into build/app
python3 -m zipapp build/app -c -p '/usr/bin/env python3' -o bin/tug-provider-acme
```

Point every platform in `plugin.yaml` at `bin/tug-provider-acme`, then run
`tug plugin package . -o dist`. Windows cannot run the file directly, so leave
it out of the platforms.

## Differences from the Go SDK

- There is no context. In one-shot mode cancellation arrives as `Canceled`,
  raised by the signal handlers that `main` installs; `run` installs none.
  In serve mode a cancelled request is flagged rather than interrupted, and
  only `sleep` raises `Canceled`, as a Go provider that ignores its context
  keeps running. Raising an exception in another thread needs a
  CPython-only call, can land in the SDK's own code after the provider has
  returned, and still cannot interrupt a blocking call.
- After SIGTERM in serve mode, requests in flight get one second to answer;
  the Go SDK waits for them all. tug kills a plugin 2 seconds after
  SIGTERM.
- `ServeRequest` and `ServeResponse`, which Go exports for hosts, have no
  Python counterpart, since a plugin never builds frames itself.
- `main` sends stray `print()` output to stderr; the Go SDK leaves stdout to
  the plugin.
- Times are `datetime`, which holds microseconds, so finer fractions in a
  request are truncated. Naive times are refused.
- Exceptions other than `ProviderError` become `Internal` with their type in
  the message (`KeyError: 'api_token'`) and their traceback on stderr.
- A response that cannot be encoded, such as one with `NaN` in an account's
  config, is sent as an `Internal` error rather than leaving stdout empty.
- Request field names must match exactly. Go also accepts them in another
  case, which tug never sends.
- Numbers in free-form account config keep Python's formatting, so `1.0`
  stays `1.0` where Go writes `1`; both are the same JSON number.

## Developing the SDK

```bash
cd sdk/python && python3 -m unittest discover   # or, from the top: make python-sdk-test
go test ./sdk/python                            # conformance and byte-for-byte checks against the Go SDK
```

[`examples/tug-provider-pyexample`](https://github.com/paingha/tugcli/tree/main/sdk/python/examples/tug-provider-pyexample)
is a complete plugin. From a checkout it finds the SDK two directories up, so
`tug plugin link sdk/python/examples/tug-provider-pyexample` works without
installing anything.
