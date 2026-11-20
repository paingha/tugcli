"""main and run: one plugin process, in one-shot or serve mode."""

from __future__ import annotations

import contextlib
import signal
import sys
from typing import Any, Optional

from ._errors import INVALID_CONFIG, UNAVAILABLE, ProviderError
from ._protocol import OP_SERVE, is_known_op
from ._provider import (
    EXIT_ERROR,
    EXIT_OK,
    EXIT_USAGE,
    Canceled,
    _Call,
    answer,
    current_call,
    error_message,
    log,
    set_current_call,
    stream_reader,
    write,
)
from ._serve import serve
from ._wire import quote

_USAGE = "usage: %s <describe|accounts|list|credential|rotate|checkAccess|serve>"


class _NoSignals:
    """What run uses in place of _SignalGuard: no signal is handled."""

    def disarm(self) -> None:
        """Does nothing."""

    def hold(self) -> Any:
        """Returns a context that does nothing."""
        return contextlib.nullcontext()


class _SignalGuard:
    """Turns the first SIGTERM or SIGINT into Canceled in the main thread
    until disarmed, then restores the previous handlers. Inside hold() the
    exception waits until the held section ends."""

    def __init__(self) -> None:
        self._armed = True
        self._held = 0
        self._pending = ""
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
        if not self._armed:
            return
        self._armed = False
        reason = "received %s" % signal.Signals(signum).name
        call = current_call()
        if call is not None:
            call.cancel(reason)
        if self._held:
            self._pending = reason
            return
        raise Canceled(reason)

    @contextlib.contextmanager
    def hold(self) -> Any:
        """Holds back Canceled until the block ends, for work a signal must
        not cut short."""
        self._held += 1
        try:
            yield
        finally:
            self._held -= 1
            if not self._held and self._pending:
                reason, self._pending = self._pending, ""
                raise Canceled(reason)  # the signal arrived inside the block

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


def _outcome(provider: Any, args: list[str], stdin: Any, stderr: Any) -> tuple[int, Any]:
    """Runs the operation args names and returns the exit code with what to
    write: the encoded response, the ProviderError to send instead, or None
    for a usage error, which sends nothing."""
    if len(args) != 1:
        return EXIT_USAGE, None
    op = args[0]
    if not is_known_op(op):
        return EXIT_USAGE, ProviderError(INVALID_CONFIG, "unknown operation %s" % quote(op))
    result = answer(provider, op, stream_reader(stdin), stderr)
    if isinstance(result, ProviderError):
        return EXIT_ERROR, result
    return EXIT_OK, result + b"\n"


def _write_error(stdout: Any, stderr: Any, prog: str, op: str, e: ProviderError) -> None:
    try:
        write(stdout, error_message(e) + b"\n")
    except (OSError, ValueError) as werr:
        log(stderr, "writing error response: %s" % werr)
    log(stderr, "%s %s: %s" % (prog, op, e))


def _run(provider: Any, args: list[str], stdin: Any, stdout: Any, stderr: Any, prog: str, signals: Any) -> int:
    if args == [OP_SERVE]:
        return serve(provider, stdin, stdout, stderr, prog, signals)
    op = args[0] if len(args) == 1 else ""
    set_current_call(_Call(op))
    try:
        try:
            code, result = _outcome(provider, args, stdin, stderr)
        finally:
            # From here on a signal must not cut the response short. One that
            # arrives before this line is caught below.
            signals.disarm()
    except Canceled as c:
        code, result = EXIT_ERROR, ProviderError(UNAVAILABLE, "%s cancelled: %s" % (op, c))
    finally:
        set_current_call(None)
    if result is None:
        log(stderr, _USAGE % prog)
        return code
    if isinstance(result, ProviderError):
        _write_error(stdout, stderr, prog, op, result)
        return code
    try:
        write(stdout, result)
    except (OSError, ValueError) as e:
        log(stderr, "writing response: %s" % e)
        return EXIT_ERROR
    return code


def _prog_name() -> str:
    return sys.argv[0] if sys.argv and sys.argv[0] else "tug-provider"


def run(provider: Any, args: list[str], stdin: Any, stdout: Any, stderr: Any) -> int:
    """Runs one operation, or a serve session, and returns the exit code:
    main with explicit arguments and streams, for tests and for hosts that
    embed a provider.

    args are the arguments after the program name, normally one operation.
    stdin and stdout are binary streams, such as io.BytesIO; text streams
    work too. stderr is a text stream. run does not handle signals.
    """
    return _run(provider, list(args), stdin, stdout, stderr, _prog_name(), _NoSignals())


def main(provider: Any, argv: Optional[list[str]] = None) -> int:
    """Runs provider as a plugin process and returns the exit code.

    The operation comes from argv (sys.argv by default), the request from
    stdin, and the response goes to stdout; errors and logs go to stderr.
    With the operation serve, main answers one request per line of stdin
    until stdin closes. SIGTERM and SIGINT cancel the work in hand: in
    one-shot mode they raise Canceled in the provider, and in serve mode
    they cancel every request in flight (see canceled), and main exits
    promptly. While the provider runs, sys.stdout points at stderr, so a
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
            return _run(provider, argv[1:], stdin, stdout, stderr, prog, guard)
    finally:
        sys.stdout = real_stdout
