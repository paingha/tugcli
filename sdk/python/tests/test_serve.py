"""Tests for serve mode: one process, one JSON line per request."""

import io
import json
import queue
import signal
import sys
import threading
import time
import unittest

from tug_provider import (
    EXIT_ERROR,
    EXIT_OK,
    INVALID_CONFIG,
    OP_SERVE,
    UNAVAILABLE,
    Cluster,
    Endpoints,
    ProviderError,
    canceled,
    decode_list,
    run,
    sleep,
)
from tug_provider._serve import MAX_SERVE_LINE

from .test_main import Fake, Full, call, plugin

LIST = '{"apiVersion":"provider/v1alpha1","kind":"ListRequest","account":{"id":"a"},"secrets":{"api_token":"good"}}'


class Lines(io.RawIOBase):
    """A stdin the test writes lines to while serve reads them."""

    def __init__(self):
        super().__init__()
        self._lines = queue.Queue()

    def readable(self):
        return True

    def send(self, line):
        self._lines.put(line.encode() + b"\n")

    def close_input(self):
        self._lines.put(b"")

    def readline(self, size=-1):
        return self._lines.get(timeout=10)


class Output(io.RawIOBase):
    """A stdout that collects lines and lets the test wait for them."""

    def __init__(self):
        super().__init__()
        self._lock = threading.Lock()
        self._data = b""
        self._arrived = threading.Condition(self._lock)

    def writable(self):
        return True

    def write(self, b):
        with self._lock:
            self._data += bytes(b)
            self._arrived.notify_all()
        return len(b)

    def lines(self):
        with self._lock:
            return self._data.splitlines()

    def wait_for(self, count, timeout=5):
        with self._lock:
            self._arrived.wait_for(lambda: self._data.count(b"\n") >= count, timeout)
            return self._data.splitlines()


def responses(lines):
    """Returns the responses by id, as (id, response) pairs in output order,
    checking that each line is a complete frame."""
    out = []
    for line in lines:
        frame = json.loads(line)
        if sorted(frame) != ["id", "response"]:
            raise AssertionError("not a serve response: %r" % line)
        out.append((frame["id"], frame["response"]))
    return out


def serve(provider, frames):
    """Serves frames, a list of lines, and returns the exit code and the
    responses."""
    out, err = io.BytesIO(), io.StringIO()
    code = run(provider, [OP_SERVE], io.BytesIO("".join(f + "\n" for f in frames).encode()), out, err)
    return code, responses(out.getvalue().splitlines())


def serve_live(provider):
    """Starts serving in a thread and returns its stdin, stdout and a
    function that waits for the exit code."""
    stdin, stdout, err = Lines(), Output(), io.StringIO()
    result = {}
    t = threading.Thread(target=lambda: result.setdefault("code", run(provider, [OP_SERVE], stdin, stdout, err)), daemon=True)
    t.start()

    def wait(timeout=5):
        t.join(timeout)
        if t.is_alive():
            raise AssertionError("serve did not return")
        return result["code"]

    return stdin, stdout, wait


class ServeTest(unittest.TestCase):
    def test_answers_as_one_shot_mode_does(self):
        tests = [
            ("describe", '{"id":"describe","op":"describe"}', "describe", ""),
            ("list", '{"id":"list","op":"list","request":%s}' % LIST, "list", LIST),
            ("bad token", '{"id":"bad","op":"list","request":{"secrets":{"api_token":"nope"}}}', "list", '{"secrets":{"api_token":"nope"}}'),
            ("credential", '{"id":"cred","op":"credential","request":{"kind":"CredentialRequest","clusterID":"c-1"}}', "credential", '{"kind":"CredentialRequest","clusterID":"c-1"}'),
            ("invalid credential", '{"id":"invalid","op":"credential","request":{"clusterID":"bad"}}', "credential", '{"clusterID":"bad"}'),
            ("wrong kind", '{"id":"kind","op":"list","request":{"kind":"CredentialRequest"}}', "list", '{"kind":"CredentialRequest"}'),
            ("empty list", '{"id":"empty","op":"list"}', "list", ""),
            ("null request", '{"id":"null","op":"credential","request":null}', "credential", ""),
            ("accounts without a request", '{"id":"accounts","op":"accounts"}', "accounts", ""),
            ("rotate", '{"id":"rotate","op":"rotate","request":{"kind":"RotateRequest","clusterID":"c"}}', "rotate", '{"kind":"RotateRequest","clusterID":"c"}'),
        ]
        code, got = serve(Full(), [frame for _, frame, _, _ in tests])
        self.assertEqual(code, EXIT_OK)
        got = dict(got)
        for name, frame, op, stdin in tests:
            with self.subTest(name):
                _, want, _ = call(Full(), op, stdin)
                self.assertEqual(json.dumps(got[json.loads(frame)["id"]], separators=(",", ":")).encode(), want.rstrip(b"\n"))

    def test_responses_are_the_one_shot_bytes(self):
        out = io.BytesIO()
        run(Fake(), [OP_SERVE], io.BytesIO(b'{"id":"a<b","op":"list","request":{"secrets":{"api_token":"good"}}}\n'), out, io.StringIO())
        _, want, _ = call(Fake(), "list", '{"secrets":{"api_token":"good"}}')
        self.assertEqual(out.getvalue(), b'{"id":"a\\u003cb","response":' + want.rstrip(b"\n") + b"}\n")

    def test_unknown_and_unreadable_lines(self):
        frames = [
            '{"id":"nope","op":"nope"}',
            '{"id":"serve","op":"serve"}',
            '{"id":"no op"}',
            "not json",
            '{"op":"describe"}',
            '{"id":"","op":"describe"}',
            '{"id":7,"op":"describe"}',
            '{"id":"x","op":"describe","cancel":"yes"}',
            '{"id":"x","op":1}',
            "[1]",
            "null",
            "",
            "   ",
            '{"id":"unknown cancel","cancel":true}',
            '{"id":"after","op":"describe"}',
        ]
        code, got = serve(Fake(), frames)
        self.assertEqual(code, EXIT_OK)
        messages = [(i, r["message"]) for i, r in got if r["kind"] == "Error"]
        self.assertEqual(
            sorted(messages),
            sorted(
                [
                    ("nope", 'unknown operation "nope"'),
                    ("serve", 'unknown operation "serve"'),
                    ("no op", 'unknown operation ""'),
                ]
                + [("", "each line must be a JSON object with an id")] * 8
            ),
        )
        self.assertTrue(all(r["code"] == INVALID_CONFIG for _, r in got if r["kind"] == "Error"))
        self.assertIn(("after", "DescribeResponse"), [(i, r["kind"]) for i, r in got])
        self.assertEqual(len(got), 12, "blank lines and a cancel for an unknown id are not answered")

    def test_answers_out_of_order(self):
        second_done = threading.Event()

        class Ordered(Fake):
            def list(self, request):
                if not second_done.wait(5):
                    raise AssertionError("the second request never ran")
                return []

            def credential(self, request):
                try:
                    return super().credential(request)
                finally:
                    second_done.set()

        stdin, stdout, wait = serve_live(Ordered())
        stdin.send('{"id":"first","op":"list","request":{}}')
        stdin.send('{"id":"second","op":"credential","request":{"clusterID":"c"}}')
        stdin.close_input()
        self.assertEqual(wait(), EXIT_OK)
        self.assertEqual([i for i, _ in responses(stdout.lines())], ["second", "first"])

    def test_requests_run_concurrently(self):
        n = 6
        barrier = threading.Barrier(n, timeout=5)
        big = [Cluster(id="c%d" % i, name="c%d" % i, endpoints=Endpoints(public="https://c%d.example" % i), labels={"k": "v" * 200}) for i in range(300)]

        class Concurrent(Fake):
            def list(self, request):
                barrier.wait()  # breaks, and the request fails, unless all n run at once
                return big

        code, got = serve(Concurrent(), ['{"id":"%d","op":"list","request":{}}' % i for i in range(n)])
        self.assertEqual(code, EXIT_OK)
        self.assertEqual(sorted(i for i, _ in got), [str(i) for i in range(n)])
        for i, r in got:
            self.assertEqual(r["kind"], "ListResponse", (i, r.get("message")))
            self.assertEqual(len(r["clusters"]), 300)

    def test_cancel_stops_one_request(self):
        started = threading.Event()

        class Slow(Fake):
            def list(self, request):
                started.set()
                sleep(30)
                return []

        stdin, stdout, wait = serve_live(Slow())
        begun = time.monotonic()
        stdin.send('{"id":"slow","op":"list","request":{}}')
        self.assertTrue(started.wait(5))
        stdin.send('{"id":"quick","op":"describe"}')
        self.assertEqual([i for i, _ in responses(stdout.wait_for(1))], ["quick"])
        stdin.send('{"id":"slow","cancel":true}')
        stdin.close_input()
        self.assertEqual(wait(), EXIT_OK)
        self.assertLess(time.monotonic() - begun, 5)
        got = dict(responses(stdout.lines()))
        self.assertEqual((got["slow"]["code"], got["slow"]["message"]), (UNAVAILABLE, "list cancelled: the host sent cancel"))

    def test_cancel_is_visible_to_the_provider(self):
        started = threading.Event()

        class Polling(Fake):
            def list(self, request):
                started.set()
                deadline = time.monotonic() + 10
                while not canceled():
                    if time.monotonic() > deadline:
                        raise AssertionError("never cancelled")
                    time.sleep(0.01)
                raise ProviderError(UNAVAILABLE, "stopped early")

        stdin, stdout, wait = serve_live(Polling())
        stdin.send('{"id":"poll","op":"list","request":{}}')
        self.assertTrue(started.wait(5))
        stdin.send('{"id":"poll","cancel":true}')
        stdin.close_input()
        self.assertEqual(wait(), EXIT_OK)
        self.assertEqual(dict(responses(stdout.lines()))["poll"]["message"], "stopped early")

    def test_eof_waits_for_requests_in_flight(self):
        class Slowish(Fake):
            def list(self, request):
                time.sleep(0.3)
                return []

        begun = time.monotonic()
        code, got = serve(Slowish(), ['{"id":"a","op":"list","request":{}}', '{"id":"b","op":"list","request":{}}'])
        self.assertEqual(code, EXIT_OK)
        self.assertEqual(sorted(i for i, _ in got), ["a", "b"])
        self.assertLess(time.monotonic() - begun, 0.6 + 0.5, "the two requests should run at once")

    def test_line_too_long(self):
        long = b'{"id":"long","op":"list","request":{"x":"' + b"x" * MAX_SERVE_LINE + b'"}}\n'
        out = io.BytesIO()
        code = run(Fake(), [OP_SERVE], io.BytesIO(b'{"id":"first","op":"describe"}\n' + long), out, io.StringIO())
        self.assertEqual(code, EXIT_ERROR)
        got = dict(responses(out.getvalue().splitlines()))
        self.assertEqual(got["first"]["kind"], "DescribeResponse")
        self.assertEqual(got[""]["code"], INVALID_CONFIG)
        self.assertIn("reading requests: a line exceeds", got[""]["message"])

    def test_sleep_and_canceled_outside_a_request(self):
        self.assertFalse(canceled())
        begun = time.monotonic()
        sleep(0.05)
        self.assertGreaterEqual(time.monotonic() - begun, 0.04)


class ServeProcessTest(unittest.TestCase):
    def test_eof_exits(self):
        p = plugin(OP_SERVE)
        out, err = p.communicate(b'{"id":"1","op":"list","request":{}}\n{"id":"2","op":"describe"}\n', timeout=30)
        self.assertEqual(p.returncode, EXIT_OK, err)
        got = dict(responses(out.splitlines()))
        self.assertEqual(len(decode_list(json.dumps(got["1"]))), 1)
        self.assertIn(b"stray output from list", err)

    @unittest.skipIf(sys.platform == "win32", "Windows has no SIGTERM; tug kills plugins there")
    def test_sigterm_cancels_everything_and_exits(self):
        with plugin(OP_SERVE) as p:
            try:
                p.stdin.write(b'{"id":"slow","op":"list","request":{"account":{"id":"slow"}}}\n')
                p.stdin.write(b'{"id":"stubborn","op":"list","request":{"account":{"id":"stubborn"}}}\n')
                p.stdin.flush()
                time.sleep(0.5)  # let both requests start; stdin stays open
                begun = time.monotonic()
                p.send_signal(signal.SIGTERM)
                p.wait(timeout=3)
                took = time.monotonic() - begun
            finally:
                p.kill()
                p.stdin.close()
            out, err = p.stdout.read(), p.stderr.read()
        self.assertEqual(p.returncode, EXIT_OK, err)
        self.assertLess(took, 2, "tug kills a plugin 2 seconds after SIGTERM")
        got = dict(responses(out.splitlines()))
        self.assertEqual(got["slow"]["message"], "list cancelled: received SIGTERM")
        self.assertNotIn("stubborn", got, "a request that ignores cancellation is left behind")
        self.assertIn(b"cleaning up", err)


if __name__ == "__main__":
    unittest.main()
