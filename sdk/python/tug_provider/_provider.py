"""The Provider base class, cancellation, and answering one request."""

from __future__ import annotations

import abc
import io
import threading
import time
import traceback
from typing import Any, Callable, Iterable, Optional, Union

from ._errors import INTERNAL, INVALID_CONFIG, UNAVAILABLE, UNIMPLEMENTED, ProviderError, as_error, find_provider_error
from ._protocol import (
    KIND_ACCOUNTS_REQUEST,
    KIND_ACCOUNTS_RESPONSE,
    KIND_CHECK_ACCESS_REQUEST,
    KIND_CHECK_ACCESS_RESPONSE,
    KIND_CREDENTIAL_REQUEST,
    KIND_CREDENTIAL_RESPONSE,
    KIND_DESCRIBE_RESPONSE,
    KIND_ERROR,
    KIND_LIST_REQUEST,
    KIND_LIST_RESPONSE,
    KIND_ROTATE_REQUEST,
    MAX_MESSAGE_BYTES,
    OP_ACCOUNTS,
    OP_CHECK_ACCESS,
    OP_CREDENTIAL,
    OP_DESCRIBE,
    OP_LIST,
    OP_ROTATE,
    OP_SERVE,
    envelope,
    supported_api_versions,
)
from ._types import (
    CAP_ACCOUNTS,
    CAP_CHECK_ACCESS,
    CAP_CREDENTIAL,
    CAP_LIST,
    CAP_ROTATE,
    CAP_SERVE,
    AccessRequest,
    Account,
    Cluster,
    Credential,
    CredentialRequest,
    Description,
    Finding,
    Info,
    ListRequest,
    sort_capabilities,
)
from ._wire import DecodeError, dumps, json_type, loads, quote

# Exit codes returned by main and run.
EXIT_OK = 0
EXIT_ERROR = 1
EXIT_USAGE = 2


class Provider(abc.ABC):
    """What a plugin implements.

    Subclasses implement info, list and credential. accounts, rotate and
    check_access are optional: override one to support it, and describe
    lists its capability. Raise ProviderError for failures tug should
    classify; any other exception reaches tug as Internal.

    In serve mode one process answers many requests, each in a thread of
    its own, so the methods may run concurrently.
    """

    @abc.abstractmethod
    def info(self) -> Info:
        """Returns the name, version and secrets, which must match
        plugin.yaml."""

    @abc.abstractmethod
    def list(self, request: ListRequest) -> Iterable[Cluster]:
        """Returns the clusters in request.account. Raise rather than return
        a partial listing: a cluster missing from a successful listing is
        marked stale."""

    @abc.abstractmethod
    def credential(self, request: CredentialRequest) -> Credential:
        """Returns a credential for the cluster request.cluster_id."""

    def accounts(self) -> Iterable[Account]:
        """Finds accounts already set up on this machine, for example
        profiles in a vendor CLI's config. Optional."""
        raise ProviderError(UNIMPLEMENTED, "accounts is not supported")

    def rotate(self, request: CredentialRequest) -> Credential:
        """Issues a new credential and revokes the old one where possible.
        Optional."""
        raise ProviderError(UNIMPLEMENTED, "rotate is not supported")

    def check_access(self, request: AccessRequest) -> Iterable[Finding]:
        """Explains why the caller may not reach a cluster, for example an
        IP allow list on the control plane. Optional."""
        raise ProviderError(UNIMPLEMENTED, "checkAccess is not supported")


# The optional operations: the method that implements each one, and its
# capability.
_OPTIONAL = (("accounts", CAP_ACCOUNTS), ("rotate", CAP_ROTATE), ("check_access", CAP_CHECK_ACCESS))


def _implements(provider: Any, method: str) -> bool:
    """Reports whether provider's class defines method itself rather than
    inheriting Provider's, which answers Unimplemented."""
    impl = getattr(type(provider), method, None)
    return callable(impl) and impl is not getattr(Provider, method)


def capabilities(provider: Any) -> list[str]:
    """Returns the capability names of provider, in describe's order: list,
    credential and serve, the ones info() declares, and one for each
    optional operation the provider overrides. Every SDK plugin can serve;
    tug uses serve mode only when plugin.yaml lists it too."""
    names = set(provider.info().capabilities) | {CAP_LIST, CAP_CREDENTIAL, CAP_SERVE}
    names.update(cap for method, cap in _OPTIONAL if _implements(provider, method))
    return sort_capabilities(names)


class Canceled(BaseException):
    """Raised when the request a provider is answering is cancelled.

    In one-shot mode main raises it in the main thread, wherever the
    provider is, when the plugin receives SIGTERM or SIGINT, which tug sends
    when an operation times out or the user presses Ctrl-C. In serve mode
    sleep raises it once the host cancels the request or the plugin is told
    to stop; see canceled.

    It derives from BaseException, as KeyboardInterrupt does, so that
    `except Exception` in a provider does not swallow it. The SDK answers
    Unavailable. Catch it only to clean up, then raise it again.
    """


class _Call:
    """One request being answered, and whether it has been cancelled."""

    def __init__(self, op: str, request_id: str = "") -> None:
        self.op = op
        self.id = request_id
        self.reason = ""
        self._event = threading.Event()

    def cancel(self, reason: str) -> None:
        """Marks the call cancelled; the first reason given is kept."""
        if not self._event.is_set():
            self.reason = reason
            self._event.set()

    def is_canceled(self) -> bool:
        """Reports whether the call has been cancelled."""
        return self._event.is_set()

    def wait(self, timeout: float) -> bool:
        """Waits up to timeout seconds for the call to be cancelled and
        reports whether it was."""
        return self._event.wait(timeout)


_local = threading.local()


def current_call() -> Optional[_Call]:
    """Returns the call this thread is answering, or None."""
    return getattr(_local, "call", None)


def set_current_call(call: Optional[_Call]) -> None:
    """Records the call this thread is answering."""
    _local.call = call


def canceled() -> bool:
    """Reports whether the request this thread is answering has been
    cancelled: in serve mode by the host or by SIGTERM, and in one-shot
    mode by SIGTERM, which main also turns into Canceled.

    In serve mode the SDK does not interrupt a provider whose request is
    cancelled: check canceled() between steps of long work, and wait with
    sleep, so that the request stops promptly. Outside a request it
    returns False.
    """
    call = current_call()
    return call is not None and call.is_canceled()


def sleep(seconds: float) -> None:
    """Waits like time.sleep, but raises Canceled as soon as the request
    this thread is answering is cancelled, and at once if it already has
    been. Use it for waits such as polling for a replacement credential."""
    call = current_call()
    if call is None:
        time.sleep(seconds)
        return
    deadline = time.monotonic() + max(0.0, seconds)
    while True:
        # Short waits let a signal handler run in the main thread even where
        # a lock wait cannot be interrupted.
        remaining = deadline - time.monotonic()
        if call.wait(min(max(0.0, remaining), 0.1)):
            raise Canceled(call.reason or "cancelled")
        if remaining <= 0.1:
            return


# A reader returns the decoded request message for the kind of request an
# operation expects, or None when an optional request is empty, and raises
# ProviderError for a request it refuses.
Reader = Callable[[str, bool], Optional[dict]]


def check_message(msg: Any, kind: str) -> dict[str, Any]:
    """Checks a decoded, non-empty request message. As in the Go SDK, a
    request may leave out apiVersion and kind, but one for another protocol
    version or operation is refused rather than misread."""
    if not isinstance(msg, dict):
        raise ProviderError(INVALID_CONFIG, "request is not JSON: %s is not an object" % json_type(msg))
    version, got = msg.get("apiVersion"), msg.get("kind")
    for name, value in (("apiVersion", version), ("kind", got)):
        if value is not None and not isinstance(value, str):
            raise ProviderError(INVALID_CONFIG, "request is not JSON: %s is %s, want a string" % (name, json_type(value)))
    if version and version not in supported_api_versions():
        raise ProviderError(
            INVALID_CONFIG,
            "unsupported apiVersion %s; this plugin speaks [%s]" % (quote(version), " ".join(supported_api_versions())),
        )
    if got and got != kind:
        raise ProviderError(INVALID_CONFIG, "expected kind %s, got %s" % (kind, quote(got)))
    return msg


def empty_request(kind: str, optional: bool) -> None:
    """Answers an empty request: None when it is optional, else the error
    both modes give."""
    if optional:
        return None
    raise ProviderError(INVALID_CONFIG, "empty request; expected %s on stdin" % kind)


def _read_all(stream: Any) -> bytes:
    if stream is None:
        return b""
    data = stream.read(MAX_MESSAGE_BYTES + 1)
    if isinstance(data, str):
        data = data.encode("utf-8", "surrogateescape")
    return bytes(data or b"")


def stream_reader(stdin: Any) -> Reader:
    """Returns a reader for one-shot mode, which reads the request from
    stdin."""

    def read(kind: str, optional: bool) -> Optional[dict]:
        try:
            data = _read_all(stdin)
        except Canceled as c:
            raise ProviderError(UNAVAILABLE, "cancelled while reading the request: %s" % c) from None
        except (OSError, ValueError) as e:
            raise ProviderError(INTERNAL, "reading request: %s" % e) from None
        if len(data) > MAX_MESSAGE_BYTES:
            raise ProviderError(INVALID_CONFIG, "request exceeds %d bytes" % MAX_MESSAGE_BYTES)
        if not data.strip():
            return empty_request(kind, optional)
        try:
            msg = loads(data)
        except ValueError as e:
            raise ProviderError(INVALID_CONFIG, "request is not JSON: %s" % e) from None
        if msg is None:  # Go reads null as an empty object
            msg = {}
        return check_message(msg, kind)

    return read


def _parse(cls: Any, msg: Optional[dict[str, Any]], kind: str) -> Any:
    try:
        return cls.from_dict(msg, "")
    except DecodeError as e:
        raise ProviderError(INVALID_CONFIG, "malformed %s: %s" % (kind, e)) from None


def _items(result: Optional[Iterable[Any]], cls: type, op: str) -> list[Any]:
    """Returns a provider's results as a list, checking their type. None is
    an empty list."""
    items = list(result or [])
    for item in items:
        if not isinstance(item, cls):
            raise ProviderError(INTERNAL, "%s returned %s, want %s" % (op, type(item).__name__, cls.__name__))
    return items


def _credential_response(cred: Any, op: str) -> dict[str, Any]:
    if not isinstance(cred, Credential):
        raise ProviderError(INTERNAL, "%s returned %s, want Credential" % (op, type(cred).__name__))
    try:
        cred.validate()
    except ValueError as e:
        raise ProviderError(INTERNAL, "provider returned an invalid credential: %s" % e) from None
    return envelope(KIND_CREDENTIAL_RESPONSE, {"credential": cred.to_dict()})


def _unsupported(provider: Any, method: str, op: str) -> None:
    if not _implements(provider, method):
        raise ProviderError(UNIMPLEMENTED, "%s is not supported" % op)


def _dispatch(provider: Any, op: str, read: Reader) -> dict[str, Any]:
    if op == OP_DESCRIBE:
        info = provider.info()
        desc = Description(
            name=info.name,
            version=info.version,
            description=info.description,
            protocols=supported_api_versions(),
            capabilities=capabilities(provider),
            secrets=list(info.secrets),
        )
        return envelope(KIND_DESCRIBE_RESPONSE, desc.to_dict())

    if op == OP_ACCOUNTS:
        _unsupported(provider, "accounts", op)
        read(KIND_ACCOUNTS_REQUEST, True)
        accounts = _items(provider.accounts(), Account, op)
        return envelope(KIND_ACCOUNTS_RESPONSE, {"accounts": [a.to_dict() for a in accounts]})

    if op == OP_LIST:
        req = _parse(ListRequest, read(KIND_LIST_REQUEST, False), KIND_LIST_REQUEST)
        clusters = _items(provider.list(req), Cluster, op)
        return envelope(KIND_LIST_RESPONSE, {"clusters": [c.to_dict() for c in clusters]})

    if op == OP_CREDENTIAL:
        req = _parse(CredentialRequest, read(KIND_CREDENTIAL_REQUEST, False), KIND_CREDENTIAL_REQUEST)
        return _credential_response(provider.credential(req), op)

    if op == OP_ROTATE:
        _unsupported(provider, "rotate", op)
        req = _parse(CredentialRequest, read(KIND_ROTATE_REQUEST, False), KIND_ROTATE_REQUEST)
        return _credential_response(provider.rotate(req), op)

    if op == OP_CHECK_ACCESS:
        _unsupported(provider, "check_access", op)
        req = _parse(AccessRequest, read(KIND_CHECK_ACCESS_REQUEST, False), KIND_CHECK_ACCESS_REQUEST)
        findings = _items(provider.check_access(req), Finding, op)
        return envelope(KIND_CHECK_ACCESS_RESPONSE, {"findings": [x.to_dict() for x in findings]})

    if op == OP_SERVE:
        raise ProviderError(INVALID_CONFIG, "serve is a mode, not a request")

    raise ProviderError(INVALID_CONFIG, "unknown operation %s" % quote(op))


def answer(provider: Any, op: str, read: Reader, stderr: Any) -> Union[bytes, ProviderError]:
    """Answers one request for op and returns the encoded response message,
    without a newline, or the ProviderError to send instead. Canceled
    propagates."""
    try:
        return dumps(_dispatch(provider, op, read))
    except ProviderError as e:
        return e
    except Exception as e:  # every failure must reach tug as an Error
        found = find_provider_error(e)
        if found is None:
            # A bug in the provider: the traceback goes to tug's log.
            log(stderr, traceback.format_exc().rstrip("\n"))
            found = as_error(e)
        return found


_log_lock = threading.Lock()


def log(stderr: Any, line: str) -> None:
    """Writes one line to stderr, which may be a text or a binary stream,
    without interleaving it with lines from other threads. Failures are
    ignored: there is nowhere left to report them."""
    if stderr is None:
        return
    with _log_lock:
        try:
            if isinstance(stderr, (io.RawIOBase, io.BufferedIOBase)):
                stderr.write((line + "\n").encode("utf-8", "backslashreplace"))
            else:
                stderr.write(line + "\n")
            stderr.flush()
        except (OSError, ValueError):
            pass


def write(stdout: Any, data: bytes) -> None:
    """Writes data to stdout, which may be a binary or a text stream."""
    if stdout is None:
        raise OSError("there is no stdout")
    if isinstance(stdout, io.TextIOBase):
        stdout.write(data.decode("utf-8"))
    else:
        stdout.write(data)
    stdout.flush()


def error_message(e: ProviderError) -> bytes:
    """Returns the encoded Error message for e, without a newline. A missing
    code is sent as Internal."""
    if not e.code:
        e.code = INTERNAL
    return dumps(envelope(KIND_ERROR, e.to_dict()))
