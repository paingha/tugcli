"""Serve mode: one plugin process answers many requests.

The host runs `<plugin> serve` and writes one JSON frame per line,
{"id", "op", "request"}, or {"id", "cancel": true} to cancel a request. Each
request is answered in a thread of its own with one line, {"id",
"response"}, whose response is exactly what one-shot mode would write for
it. Answers may come out of order.
"""

from __future__ import annotations

import threading
import time
from typing import Any, Optional

from ._errors import INVALID_CONFIG, UNAVAILABLE, ProviderError, as_error
from ._protocol import MAX_MESSAGE_BYTES, OP_SERVE, is_known_op
from ._provider import (
    EXIT_ERROR,
    EXIT_OK,
    Canceled,
    Reader,
    _Call,
    answer,
    check_message,
    empty_request,
    error_message,
    log,
    set_current_call,
    write,
)
from ._wire import dumps, loads, quote

MAX_SERVE_LINE = MAX_MESSAGE_BYTES + (64 << 10)
"""The longest line serve mode reads: a message and its frame."""

CANCEL_GRACE = 1.0
"""How long, in seconds, requests in flight have to answer after SIGTERM
before the plugin exits without them. tug kills the plugin 2 seconds after
SIGTERM."""

_UNREADABLE = "each line must be a JSON object with an id"


def parse_frame(line: bytes) -> Optional[tuple[str, str, bool, Any]]:
    """Returns the id, op, cancel flag and decoded request of a frame, or
    None for a line that is not a JSON object with a non-empty string id, or
    whose fields have the wrong types, as Go's decoder refuses them."""
    try:
        frame = loads(line)
    except ValueError:
        return None
    if not isinstance(frame, dict):
        return None
    request_id, op, cancel = frame.get("id"), frame.get("op"), frame.get("cancel")
    if not isinstance(request_id, str) or not request_id:
        return None
    if (op is not None and not isinstance(op, str)) or (cancel is not None and not isinstance(cancel, bool)):
        return None
    return request_id, op or "", bool(cancel), frame.get("request")


def served_reader(request: Any) -> Reader:
    """Returns a reader for a request that came in a frame. A missing or
    null request is empty."""

    def read(kind: str, optional: bool) -> Optional[dict]:
        if request is None:
            return empty_request(kind, optional)
        return check_message(request, kind)

    return read


class _Session:
    """The requests in flight in one serve session, and its output."""

    def __init__(self, provider: Any, stdout: Any, stderr: Any, prog: str) -> None:
        self.provider = provider
        self.stdout = stdout
        self.stderr = stderr
        self.prog = prog
        self._lock = threading.Lock()  # guards _calls and _active
        self._idle = threading.Condition(self._lock)
        self._calls: dict[str, _Call] = {}
        self._active = 0
        self._out = threading.Lock()  # one line at a time on stdout
        self._closed = False

    def send(self, request_id: str, message: bytes) -> None:
        """Writes one response line, unless the session is closed."""
        line = b'{"id":' + dumps(request_id) + b',"response":' + message + b"}\n"
        with self._out:
            if self._closed:
                return
            try:
                write(self.stdout, line)
            except (OSError, ValueError) as e:
                log(self.stderr, "writing response: %s" % e)

    def send_error(self, request_id: str, op: str, e: ProviderError) -> None:
        """Answers a request with an Error and logs it."""
        self.send(request_id, error_message(e))
        log(self.stderr, "%s %s: %s" % (self.prog, op, e))

    def handle(self, line: bytes) -> None:
        """Acts on one line from the host."""
        if not line.strip():
            return
        frame = parse_frame(line)
        if frame is None:
            self.send_error("", OP_SERVE, ProviderError(INVALID_CONFIG, _UNREADABLE))
            return
        request_id, op, cancel, request = frame
        if cancel:
            with self._lock:
                call = self._calls.get(request_id)
            if call is not None:
                call.cancel("the host sent cancel")
            return
        if op == OP_SERVE or not is_known_op(op):
            self.send_error(request_id, op, ProviderError(INVALID_CONFIG, "unknown operation %s" % quote(op)))
            return
        call = _Call(op, request_id)
        with self._lock:
            self._calls[request_id] = call  # a reused id replaces the earlier request, as in Go
            self._active += 1
        thread = threading.Thread(target=self._work, args=(call, request), name="tug-provider %s %s" % (op, request_id), daemon=True)
        try:
            thread.start()
        except RuntimeError as e:  # the system is out of threads
            self._done(call)
            self.send_error(request_id, op, ProviderError(UNAVAILABLE, "starting a thread for the request: %s" % e))

    def _work(self, call: _Call, request: Any) -> None:
        set_current_call(call)
        try:
            try:
                if call.is_canceled():
                    raise Canceled(call.reason)
                result = answer(self.provider, call.op, served_reader(request), self.stderr)
            except Canceled as c:
                result = ProviderError(UNAVAILABLE, "%s cancelled: %s" % (call.op, call.reason or str(c) or "cancelled"))
            except BaseException as e:  # SystemExit and the like still get an answer
                result = as_error(e)
            if isinstance(result, ProviderError):
                self.send_error(call.id, call.op, result)
            else:
                self.send(call.id, result)
        finally:
            set_current_call(None)
            self._done(call)

    def _done(self, call: _Call) -> None:
        with self._lock:
            if self._calls.get(call.id) is call:
                del self._calls[call.id]
            self._active -= 1
            self._idle.notify_all()

    def cancel_all(self, reason: str) -> None:
        """Cancels every request in flight."""
        with self._lock:
            calls = list(self._calls.values())
        for call in calls:
            call.cancel(reason)

    def wait(self, timeout: Optional[float] = None) -> bool:
        """Waits for the requests in flight to be answered and reports
        whether they were. It wakes often, so that a signal handler can
        run even where lock waits cannot be interrupted."""
        deadline = None if timeout is None else time.monotonic() + timeout
        with self._idle:
            while self._active:
                step = 0.2
                if deadline is not None:
                    step = min(step, deadline - time.monotonic())
                    if step <= 0:
                        return False
                self._idle.wait(step)
        return True

    def close(self) -> None:
        """Stops all further output, after a line being written, if any."""
        acquired = self._out.acquire(timeout=1.0)
        self._closed = True
        if acquired:
            self._out.release()


def _read_line(stdin: Any) -> Optional[bytes]:
    """Reads one line, or returns None when stdin is closed. A line longer
    than MAX_SERVE_LINE raises ValueError."""
    if stdin is None:
        return None
    line = stdin.readline(MAX_SERVE_LINE + 2)
    if isinstance(line, str):
        line = line.encode("utf-8", "surrogateescape")
    if not line:
        return None
    if len(line.rstrip(b"\r\n")) > MAX_SERVE_LINE:
        raise ValueError("a line exceeds %d bytes" % MAX_SERVE_LINE)
    return line


def serve(provider: Any, stdin: Any, stdout: Any, stderr: Any, prog: str, signals: Any) -> int:
    """Answers frames from stdin until it closes, then waits for the requests
    in flight and returns EXIT_OK. When signals raises Canceled, it cancels
    every request, gives them CANCEL_GRACE seconds to answer, and returns
    EXIT_OK without the rest. A line it cannot read ends the session with
    an Error and EXIT_ERROR."""
    session = _Session(provider, stdout, stderr, prog)
    code = EXIT_OK
    try:
        while True:
            try:
                line = _read_line(stdin)
            except (OSError, ValueError) as e:
                with signals.hold():
                    session.send_error("", OP_SERVE, ProviderError(INVALID_CONFIG, "reading requests: %s" % e))
                code = EXIT_ERROR
                break
            if line is None:
                break  # stdin closed: finish what is in flight, then exit
            # A signal while a line is handled waits until it is, so that a
            # request is never half started and a line never half written.
            with signals.hold():
                session.handle(line)
        session.wait()
    except Canceled as c:
        session.cancel_all(str(c) or "cancelled")
        session.wait(CANCEL_GRACE)
        session.close()
        return EXIT_OK
    return code
