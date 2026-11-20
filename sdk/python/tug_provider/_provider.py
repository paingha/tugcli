"""The Provider base class and main, which runs one as a plugin process."""

from __future__ import annotations

import abc
import io
import signal
import sys
import traceback
from typing import Any, Callable, Iterable, Optional

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
    is_known_op,
    supported_api_versions,
)
from ._types import (
    CAP_ACCOUNTS,
    CAP_CHECK_ACCESS,
    CAP_CREDENTIAL,
    CAP_LIST,
    CAP_ROTATE,
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

_USAGE = "usage: %s <describe|accounts|list|credential|rotate|checkAccess>"


class Provider(abc.ABC):
    """What a plugin implements.

    Subclasses implement info, list and credential. accounts, rotate and
    check_access are optional: override one to support it, and describe
    lists its capability. Raise ProviderError for failures tug should
    classify; any other exception reaches tug as Internal.
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
    """Returns the capability names of provider, in describe's order: list
    and credential, the ones info() declares, and one for each optional
    operation the provider overrides."""
    names = set(provider.info().capabilities) | {CAP_LIST, CAP_CREDENTIAL}
    names.update(cap for method, cap in _OPTIONAL if _implements(provider, method))
    return sort_capabilities(names)


class Canceled(BaseException):
    """Raised in the main thread when main receives SIGTERM or SIGINT, which
    tug sends when an operation times out or the user presses Ctrl-C.

    It derives from BaseException, as KeyboardInterrupt does, so that
    `except Exception` in a provider does not swallow it. main answers
    Unavailable and exits 1. Catch it only to clean up, then raise it again.
    """


def _read_all(stream: Any) -> bytes:
    if stream is None:
        return b""
    data = stream.read(MAX_MESSAGE_BYTES + 1)
    if isinstance(data, str):
        data = data.encode("utf-8", "surrogateescape")
    return bytes(data or b"")


def _read_request(stdin: Any, kind: str, optional: bool) -> Optional[dict[str, Any]]:
    """Reads and checks one request message. When optional is true an empty
    stdin is accepted and None returned. As in the Go SDK, a request may
    leave out apiVersion and kind, but one for another protocol version or
    operation is refused rather than misread."""
    try:
        data = _read_all(stdin)
    except Canceled as c:
        raise ProviderError(UNAVAILABLE, "cancelled while reading the request: %s" % c) from None
    except (OSError, ValueError) as e:
        raise ProviderError(INTERNAL, "reading request: %s" % e) from None
    if len(data) > MAX_MESSAGE_BYTES:
        raise ProviderError(INVALID_CONFIG, "request exceeds %d bytes" % MAX_MESSAGE_BYTES)
    if not data.strip():
        if optional:
            return None
        raise ProviderError(INVALID_CONFIG, "empty request; expected %s on stdin" % kind)
    try:
        msg = loads(data)
    except ValueError as e:
        raise ProviderError(INVALID_CONFIG, "request is not JSON: %s" % e) from None
    if msg is None:  # Go reads null as an empty object
        msg = {}
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


def _dispatch(provider: Any, op: str, stdin: Any) -> dict[str, Any]:
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
        _read_request(stdin, KIND_ACCOUNTS_REQUEST, True)
        accounts = _items(provider.accounts(), Account, op)
        return envelope(KIND_ACCOUNTS_RESPONSE, {"accounts": [a.to_dict() for a in accounts]})

    if op == OP_LIST:
        req = _parse(ListRequest, _read_request(stdin, KIND_LIST_REQUEST, False), KIND_LIST_REQUEST)
        clusters = _items(provider.list(req), Cluster, op)
        return envelope(KIND_LIST_RESPONSE, {"clusters": [c.to_dict() for c in clusters]})

    if op == OP_CREDENTIAL:
        req = _parse(CredentialRequest, _read_request(stdin, KIND_CREDENTIAL_REQUEST, False), KIND_CREDENTIAL_REQUEST)
        return _credential_response(provider.credential(req), op)

    if op == OP_ROTATE:
        _unsupported(provider, "rotate", op)
        req = _parse(CredentialRequest, _read_request(stdin, KIND_ROTATE_REQUEST, False), KIND_ROTATE_REQUEST)
        return _credential_response(provider.rotate(req), op)

    if op == OP_CHECK_ACCESS:
        _unsupported(provider, "check_access", op)
        req = _parse(AccessRequest, _read_request(stdin, KIND_CHECK_ACCESS_REQUEST, False), KIND_CHECK_ACCESS_REQUEST)
        findings = _items(provider.check_access(req), Finding, op)
        return envelope(KIND_CHECK_ACCESS_RESPONSE, {"findings": [x.to_dict() for x in findings]})

    if op == OP_SERVE:
        raise ProviderError(UNIMPLEMENTED, "serve mode is not supported by this SDK version")

    raise ProviderError(INVALID_CONFIG, "unknown operation %s" % quote(op))


def _log(stderr: Any, line: str) -> None:
    """Writes one line to stderr, which may be a text or a binary stream.
    Failures are ignored: there is nowhere left to report them."""
    if stderr is None:
        return
    try:
        if isinstance(stderr, (io.RawIOBase, io.BufferedIOBase)):
            stderr.write((line + "\n").encode("utf-8", "backslashreplace"))
        else:
            stderr.write(line + "\n")
        stderr.flush()
    except (OSError, ValueError):
        pass


def _write(stdout: Any, data: bytes) -> None:
    """Writes the response to stdout, which may be a binary or a text
    stream."""
    if stdout is None:
        raise OSError("there is no stdout")
    if isinstance(stdout, io.TextIOBase):
        stdout.write(data.decode("utf-8"))
    else:
        stdout.write(data)
    stdout.flush()


def _error_message(e: ProviderError) -> bytes:
    return dumps(envelope(KIND_ERROR, e.to_dict())) + b"\n"


def _write_error(stdout: Any, stderr: Any, prog: str, op: str, e: ProviderError) -> None:
    if not e.code:
        e.code = INTERNAL
    try:
        _write(stdout, _error_message(e))
    except (OSError, ValueError) as werr:
        _log(stderr, "writing error response: %s" % werr)
    _log(stderr, "%s %s: %s" % (prog, op, e))


def _outcome(provider: Any, args: list[str], stdin: Any, stderr: Any) -> tuple[int, Any]:
    """Runs the operation args names and returns the exit code with what to
    write: the encoded response, the ProviderError to send instead, or None
    for a usage error, which sends nothing."""
    if len(args) != 1:
        return EXIT_USAGE, None
    op = args[0]
    if not is_known_op(op):
        return EXIT_USAGE, ProviderError(INVALID_CONFIG, "unknown operation %s" % quote(op))
    try:
        return EXIT_OK, dumps(_dispatch(provider, op, stdin)) + b"\n"
    except ProviderError as e:
        return EXIT_ERROR, e
    except Exception as e:  # every failure must reach tug as an Error
        found = find_provider_error(e)
        if found is None:
            # A bug in the provider: the traceback goes to tug's log.
            _log(stderr, traceback.format_exc().rstrip("\n"))
            found = as_error(e)
        return EXIT_ERROR, found


def _run(provider: Any, args: list[str], stdin: Any, stdout: Any, stderr: Any, prog: str, disarm: Callable[[], None]) -> int:
    op = args[0] if len(args) == 1 else ""
    try:
        try:
            code, result = _outcome(provider, args, stdin, stderr)
        finally:
            # From here on a signal must not cut the response short. One that
            # arrives before this line is caught below.
            disarm()
    except Canceled as c:
        code, result = EXIT_ERROR, ProviderError(UNAVAILABLE, "%s cancelled: %s" % (op, c))
    if result is None:
        _log(stderr, _USAGE % prog)
        return code
    if isinstance(result, ProviderError):
        _write_error(stdout, stderr, prog, op, result)
        return code
    try:
        _write(stdout, result)
    except (OSError, ValueError) as e:
        _log(stderr, "writing response: %s" % e)
        return EXIT_ERROR
    return code


def _prog_name() -> str:
    return sys.argv[0] if sys.argv and sys.argv[0] else "tug-provider"


def run(provider: Any, args: list[str], stdin: Any, stdout: Any, stderr: Any) -> int:
    """Runs one operation and returns the exit code: main with explicit
    arguments and streams, for tests and for hosts that embed a provider.

    args are the arguments after the program name, normally one operation.
    stdin and stdout are binary streams, such as io.BytesIO; text streams
    work too. stderr is a text stream. run does not handle signals.
    """
    return _run(provider, list(args), stdin, stdout, stderr, _prog_name(), lambda: None)


class _SignalGuard:
    """Turns the first SIGTERM or SIGINT into Canceled in the main thread
    until disarmed, then restores the previous handlers."""

    def __init__(self) -> None:
        self._armed = True
        self._previous: dict[int, Any] = {}

    def __enter__(self) -> _SignalGuard:
        for name in ("SIGTERM", "SIGINT"):
            signum = getattr(signal, name, None)
            if signum is None:
                continue
            try:
                self._previous[signum] = signal.signal(signum, self._handle)
            except (OSError, ValueError):
                pass  # not the main thread, or not a signal this platform delivers
        return self

    def _handle(self, signum: int, _frame: Any) -> None:
        if self._armed:
            self._armed = False
            raise Canceled("received %s" % signal.Signals(signum).name)

    def disarm(self) -> None:
        """Stops turning signals into Canceled."""
        self._armed = False

    def __exit__(self, *exc: Any) -> None:
        self._armed = False
        for signum, previous in self._previous.items():
            try:
                signal.signal(signum, previous if previous is not None else signal.SIG_DFL)
            except (OSError, ValueError):
                pass


def main(provider: Any, argv: Optional[list[str]] = None) -> int:
    """Runs provider as a plugin process and returns the exit code.

    The operation comes from argv (sys.argv by default), the request from
    stdin, and the response goes to stdout; errors and logs go to stderr.
    SIGTERM and SIGINT raise Canceled in the provider, and main answers
    Unavailable. While the provider runs, sys.stdout points at stderr, so a
    stray print() reaches tug's log instead of breaking the response. A
    plugin's script typically ends:

        if __name__ == "__main__":
            sys.exit(main(MyProvider()))
    """
    argv = list(sys.argv if argv is None else argv)
    prog = argv[0] if argv and argv[0] else "tug-provider"
    stdin = getattr(sys.stdin, "buffer", sys.stdin)
    stdout = getattr(sys.stdout, "buffer", sys.stdout)
    stderr = sys.stderr
    real_stdout = sys.stdout
    sys.stdout = sys.stderr
    try:
        with _SignalGuard() as guard:
            return _run(provider, argv[1:], stdin, stdout, stderr, prog, guard.disarm)
    finally:
        sys.stdout = real_stdout
