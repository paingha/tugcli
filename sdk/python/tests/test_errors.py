"""Tests for ProviderError and the HTTP status mapping."""

import email.message
import http.client
import io
import unittest
from datetime import timedelta

from tug_provider import (
    INTERNAL,
    INVALID_CONFIG,
    NOT_FOUND,
    PERMISSION_DENIED,
    RATE_LIMITED,
    UNAUTHENTICATED,
    UNAVAILABLE,
    ProviderError,
    as_error,
    code_of,
    status_error,
)


def http_message(raw):
    return http.client.parse_headers(io.BytesIO(raw))


class ProviderErrorTest(unittest.TestCase):
    def test_str_has_code_and_message_but_not_hint(self):
        e = ProviderError(UNAUTHENTICATED, "bad token", hint="make a new one")
        self.assertEqual(str(e), "Unauthenticated: bad token")
        self.assertEqual((e.code, e.message, e.hint), (UNAUTHENTICATED, "bad token", "make a new one"))

    def test_with_hint_copies(self):
        e = ProviderError(NOT_FOUND, "no cluster", retry_after_seconds=3)
        h = e.with_hint("run tug sync")
        self.assertEqual((e.hint, h.hint, h.code, h.message, h.retry_after_seconds), ("", "run tug sync", NOT_FOUND, "no cluster", 3))

    def test_retry_after(self):
        self.assertEqual(ProviderError(RATE_LIMITED, "x", retry_after_seconds=30).retry_after, timedelta(seconds=30))
        self.assertEqual(ProviderError(RATE_LIMITED, "x").retry_after, timedelta(0))

    def test_code_of_and_as_error(self):
        pe = ProviderError(PERMISSION_DENIED, "no")
        try:
            try:
                raise pe
            except ProviderError as inner:
                raise RuntimeError("listing clusters") from inner
        except RuntimeError as outer:
            wrapped = outer
        try:
            try:
                raise pe
            except ProviderError:
                raise RuntimeError("while handling") from None
        except RuntimeError as outer:
            unrelated = outer
        tests = [
            ("provider error", pe, PERMISSION_DENIED, "no"),
            ("raised from a provider error", wrapped, PERMISSION_DENIED, "no"),
            ("raised while handling one", unrelated, INTERNAL, "RuntimeError: while handling"),
            ("other exception", KeyError("api_token"), INTERNAL, "KeyError: 'api_token'"),
            ("exception without a message", ValueError(), INTERNAL, "ValueError"),
        ]
        for name, err, want_code, want_message in tests:
            with self.subTest(name):
                self.assertEqual(code_of(err), want_code)
                self.assertEqual(as_error(err).message, want_message)
        self.assertIs(as_error(pe), pe)

    def test_to_dict(self):
        self.assertEqual(
            ProviderError(RATE_LIMITED, "slow", hint="wait", retry_after_seconds=30).to_dict(),
            {"code": "RateLimited", "message": "slow", "hint": "wait", "retryAfterSeconds": 30},
        )
        self.assertEqual(ProviderError("", "x").to_dict(), {"code": "Internal", "message": "x"})
        self.assertEqual(ProviderError.from_dict({"message": "x"}).code, INTERNAL)


class StatusErrorTest(unittest.TestCase):
    def test_codes(self):
        tests = [
            (401, UNAUTHENTICATED),
            (403, PERMISSION_DENIED),
            (404, NOT_FOUND),
            (429, RATE_LIMITED),
            (500, UNAVAILABLE),
            (503, UNAVAILABLE),
            (408, UNAVAILABLE),
            (400, INVALID_CONFIG),
            (422, INVALID_CONFIG),
            (418, INTERNAL),
        ]
        for status, want in tests:
            with self.subTest(status):
                got = status_error(status, "", {"Retry-After": "30"})
                self.assertEqual(got.code, want, "status_error(%d) = %r" % (status, got))
                self.assertNotEqual(got.message, "", "status_error(%d) has no message" % status)

    def test_message_is_trimmed(self):
        self.assertEqual(status_error(404, "  GET /v1/clusters: gone\n").message, "GET /v1/clusters: gone")
        self.assertEqual(status_error(599).message, "")

    def test_retry_after(self):
        msg = email.message.Message()
        msg["Retry-After"] = "30"
        tests = [
            ("http.client message", http_message(b"retry-after: 30\r\n\r\n"), 30),
            ("email message", msg, 30),
            ("dict in another case", {"retry-after": "30"}, 30),
            ("list value", {"Retry-After": ["30", "60"]}, 30),
            ("with a sign", {"Retry-After": "+30"}, 30),
            ("a date", {"Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT"}, 0),
            ("spaces", {"Retry-After": " 30"}, 0),
            ("negative", {"Retry-After": "-5"}, 0),
            ("too large", {"Retry-After": "99999999999999999999"}, 0),
            ("missing", {}, 0),
            ("no headers", None, 0),
        ]
        for name, headers, want in tests:
            with self.subTest(name):
                self.assertEqual(status_error(429, "slow down", headers).retry_after_seconds, want)
        self.assertEqual(status_error(503, "", {"Retry-After": "30"}).retry_after_seconds, 0)


if __name__ == "__main__":
    unittest.main()
