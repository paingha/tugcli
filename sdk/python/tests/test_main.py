"""Tests for run and main: one operation per call, as tug runs a plugin."""

import io
import json
import os
import signal
import subprocess
import sys
import time
import unittest
from datetime import datetime, timezone

import tug_provider
from tug_provider import (
    API_VERSION,
    EXIT_ERROR,
    EXIT_OK,
    EXIT_USAGE,
    INTERNAL,
    INVALID_CONFIG,
    MAX_MESSAGE_BYTES,
    OP_ACCOUNTS,
    OP_CHECK_ACCESS,
    OP_CREDENTIAL,
    OP_DESCRIBE,
    OP_LIST,
    OP_ROTATE,
    SEVERITY_ERROR,
    UNAUTHENTICATED,
    UNAVAILABLE,
    UNIMPLEMENTED,
    AccessRequest,
    Account,
    Canceled,
    Cluster,
    Credential,
    CredentialRequest,
    Endpoints,
    Finding,
    Info,
    ListRequest,
    Provider,
    ProviderError,
    SecretSpec,
    capabilities,
    code_of,
    decode_accounts,
    decode_check_access,
    decode_credential,
    decode_describe,
    decode_list,
    encode_request,
    run,
    token,
)

SDK = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FIXTURE = os.path.join(SDK, "tests", "fixture_plugin.py")


class Fake(Provider):
    def info(self):
        return Info(name="fake", version="1.2.3", description="Fake clusters", secrets=[SecretSpec("api_token")])

    def list(self, request):
        if request.secrets.get("api_token") != "good":
            raise ProviderError(UNAUTHENTICATED, "bad token", hint="use the good one")
        return [Cluster(id="c-1", name="one", endpoints=Endpoints(public="https://one"), ca_data=b"-----BEGIN")]

    def credential(self, request):
        if request.cluster_id == "bad":
            return Credential(type="token")  # invalid: no token
        if request.cluster_id == "wrong type":
            return {"type": "token", "token": "t"}
        if request.cluster_id == "bug":
            return {}["missing"]
        return token("tok-" + request.cluster_id, datetime(2030, 1, 1, tzinfo=timezone.utc))


class Full(Fake):
    """Implements every optional operation."""

    def accounts(self):
        return [Account("work", description="profile work", config={"profile": "work"})]

    def rotate(self, request):
        return token("rotated-" + request.cluster_id, None)

    def check_access(self, request):
        return [Finding(SEVERITY_ERROR, "allow list lacks " + request.egress_ip, fix="add %s/32" % request.egress_ip)]


class Unreadable(io.RawIOBase):
    """A stdin that fails the test if anything reads it."""

    def readable(self):
        return True

    def read(self, size=-1):
        raise AssertionError("stdin was read")


class Cancelling(io.RawIOBase):
    """A stdin whose read is interrupted, as by SIGTERM in main."""

    def readable(self):
        return True

    def read(self, size=-1):
        raise Canceled("received SIGTERM")


def call(provider, args, stdin=b""):
    """Runs provider and returns the exit code, stdout and stderr."""
    if isinstance(args, str):
        args = [args]
    if isinstance(stdin, (bytes, str)):
        stdin = io.BytesIO(stdin.encode() if isinstance(stdin, str) else stdin)
    out, err = io.BytesIO(), io.StringIO()
    code = run(provider, args, stdin, out, err)
    return code, out.getvalue(), err.getvalue()


def error_of(stdout):
    msg = json.loads(stdout)
    if msg.get("kind") != "Error":
        raise AssertionError("not an Error: %s" % stdout)
    return msg


class RunTest(unittest.TestCase):
    def test_describe(self):
        code, out, _ = call(Fake(), OP_DESCRIBE, stdin=Unreadable())
        self.assertEqual(code, EXIT_OK, out)
        d = decode_describe(out)
        self.assertEqual((d.name, d.version, d.description), ("fake", "1.2.3", "Fake clusters"))
        self.assertEqual(d.protocols, [API_VERSION])
        self.assertEqual(d.capabilities, ["list", "credential", "serve"])
        self.assertEqual(
            out,
            b'{"apiVersion":"provider/v1alpha1","kind":"DescribeResponse","name":"fake","version":"1.2.3",'
            b'"description":"Fake clusters","protocols":["provider/v1alpha1"],"capabilities":["list","credential","serve"],'
            b'"secrets":[{"name":"api_token"}]}\n',
        )

    def test_capabilities_come_from_overrides(self):
        class Serving(Fake):
            def info(self):
                return Info(name="serving", capabilities=["serve"])

            def rotate(self, request):
                return token("t", None)

        self.assertEqual(capabilities(Fake()), ["list", "credential", "serve"])
        self.assertEqual(capabilities(Full()), ["accounts", "list", "credential", "rotate", "checkAccess", "serve"])
        self.assertEqual(capabilities(Serving()), ["list", "credential", "rotate", "serve"])

    def test_list_round_trip(self):
        req = '{"apiVersion":"provider/v1alpha1","kind":"ListRequest","account":{"id":"work","config":{"org":"acme"}},"secrets":{"api_token":"good"}}'
        code, out, _ = call(Fake(), OP_LIST, req)
        self.assertEqual(code, EXIT_OK, out)
        clusters = decode_list(out)
        self.assertEqual([(c.id, c.ca_data) for c in clusters], [("c-1", b"-----BEGIN")])
        self.assertIn(b'"caData":"LS0tLS1CRUdJTg=="', out)

    def test_errors_are_json_with_non_zero_exit(self):
        code, out, err = call(Fake(), OP_LIST, '{"kind":"ListRequest","secrets":{"api_token":"nope"}}')
        self.assertEqual(code, EXIT_ERROR)
        with self.assertRaises(ProviderError) as cm:
            decode_list(out)
        self.assertEqual((cm.exception.code, cm.exception.hint), (UNAUTHENTICATED, "use the good one"))
        self.assertEqual(out.count(b"\n"), 1)
        self.assertIn("list: Unauthenticated: bad token", err)

    def test_unimplemented_and_unknown(self):
        for op in (OP_ACCOUNTS, OP_ROTATE, OP_CHECK_ACCESS):
            with self.subTest(op):
                code, out, _ = call(Fake(), op, stdin=Unreadable())
                self.assertEqual(code, EXIT_ERROR)
                self.assertEqual(error_of(out)["code"], UNIMPLEMENTED)
        code, out, _ = call(Fake(), "bogus")
        self.assertEqual(code, EXIT_USAGE)
        self.assertEqual(error_of(out), {"apiVersion": API_VERSION, "kind": "Error", "code": INVALID_CONFIG, "message": 'unknown operation "bogus"'})

    def test_usage(self):
        for args in ([], [OP_LIST, OP_LIST]):
            with self.subTest(args):
                code, out, err = call(Fake(), args)
                self.assertEqual((code, out), (EXIT_USAGE, b""))
                self.assertIn("usage: ", err)

    def test_optional_operations(self):
        code, out, _ = call(Full(), OP_ACCOUNTS)
        self.assertEqual(code, EXIT_OK, out)
        self.assertEqual(decode_accounts(out), [Account("work", description="profile work", config={"profile": "work"})])
        code, out, _ = call(Full(), OP_ACCOUNTS, encode_request(OP_ACCOUNTS))
        self.assertEqual(code, EXIT_OK, out)

        code, out, _ = call(Full(), OP_ROTATE, encode_request(OP_ROTATE, CredentialRequest(cluster_id="c-1")))
        self.assertEqual(code, EXIT_OK, out)
        self.assertEqual(decode_credential(out).token, "rotated-c-1")

        req = AccessRequest(account=Account("work"), cluster=Cluster(id="1", name="prod", ca_data=b"CA"), egress_ip="198.51.100.9")
        code, out, _ = call(Full(), OP_CHECK_ACCESS, encode_request(OP_CHECK_ACCESS, req))
        self.assertEqual(code, EXIT_OK, out)
        self.assertEqual(decode_check_access(out), [Finding(SEVERITY_ERROR, "allow list lacks 198.51.100.9", fix="add 198.51.100.9/32")])

    def test_empty_results_are_empty_arrays(self):
        class Empty(Full):
            def list(self, request):
                return None

            def accounts(self):
                return iter(())

        self.assertIn(b'"clusters":[]', call(Empty(), OP_LIST, "{}")[1])
        self.assertIn(b'"accounts":[]', call(Empty(), OP_ACCOUNTS)[1])

    def test_credential_validated(self):
        code, out, _ = call(Fake(), OP_CREDENTIAL, '{"kind":"CredentialRequest","clusterID":"c-1"}')
        self.assertEqual(code, EXIT_OK, out)
        c = decode_credential(out)
        self.assertEqual((c.token, c.expires_at), ("tok-c-1", datetime(2030, 1, 1, tzinfo=timezone.utc)))
        self.assertIn(b'"expiresAt":"2030-01-01T00:00:00Z"', out)

        code, out, _ = call(Fake(), OP_CREDENTIAL, '{"kind":"CredentialRequest","clusterID":"bad"}')
        self.assertEqual(code, EXIT_ERROR)
        self.assertEqual(error_of(out)["message"], "provider returned an invalid credential: token credential has no token")

    def test_provider_bugs_are_internal(self):
        code, out, err = call(Fake(), OP_CREDENTIAL, '{"clusterID":"wrong type"}')
        self.assertEqual((code, error_of(out)["code"]), (EXIT_ERROR, INTERNAL))
        self.assertIn("credential returned dict, want Credential", error_of(out)["message"])

        code, out, err = call(Fake(), OP_CREDENTIAL, '{"clusterID":"bug"}')
        self.assertEqual((code, error_of(out)["message"]), (EXIT_ERROR, "KeyError: 'missing'"))
        self.assertIn("Traceback", err)

        class Unencodable(Fake):
            def list(self, request):
                return [Cluster(id="c", name="c", credential=token("t", None)), "not a cluster"]

        code, out, _ = call(Unencodable(), OP_LIST, "{}")
        self.assertEqual(error_of(out)["message"], "list returned str, want Cluster")

        class NaiveExpiry(Fake):
            def credential(self, request):
                return Credential(type="token", token="t", expires_at=datetime(2030, 1, 1))

        code, out, _ = call(NaiveExpiry(), OP_CREDENTIAL, "{}")
        self.assertEqual((code, error_of(out)["code"]), (EXIT_ERROR, INTERNAL))
        self.assertIn("no time zone", error_of(out)["message"])

        class BadConfig(Full):
            def accounts(self):
                return [Account("a", config={"n": float("nan")})]

        code, out, _ = call(BadConfig(), OP_ACCOUNTS)
        self.assertEqual((code, error_of(out)["code"]), (EXIT_ERROR, INTERNAL))

    def test_request_checks(self):
        tests = [
            ("wrong kind", OP_LIST, '{"kind":"CredentialRequest"}', 'expected kind ListRequest, got "CredentialRequest"'),
            ("rotate as credential", OP_ROTATE, '{"kind":"CredentialRequest"}', 'expected kind RotateRequest, got "CredentialRequest"'),
            (
                "other version",
                OP_LIST,
                '{"apiVersion":"provider/v9","kind":"ListRequest"}',
                'unsupported apiVersion "provider/v9"; this plugin speaks [provider/v1alpha1]',
            ),
            ("empty", OP_LIST, "", "empty request; expected ListRequest on stdin"),
            ("blank", OP_CREDENTIAL, " \n\t", "empty request; expected CredentialRequest on stdin"),
            ("empty rotate", OP_ROTATE, "", "empty request; expected RotateRequest on stdin"),
            ("empty checkAccess", OP_CHECK_ACCESS, "", "empty request; expected CheckAccessRequest on stdin"),
            ("not JSON", OP_LIST, "{", "request is not JSON"),
            ("trailing data", OP_LIST, "{} {}", "request is not JSON"),
            ("NaN", OP_LIST, '{"secrets":NaN}', "request is not JSON"),
            ("an array", OP_LIST, "[]", "request is not JSON"),
            ("kind not a string", OP_LIST, '{"kind":1}', "request is not JSON"),
            ("wrong field type", OP_LIST, '{"secrets":{"api_token":1}}', "malformed ListRequest: secrets.api_token is a number, want a string"),
            ("accounts wrong kind", OP_ACCOUNTS, '{"kind":"ListRequest"}', "expected kind AccountsRequest"),
            ("too large", OP_LIST, b" " * (MAX_MESSAGE_BYTES + 1), "request exceeds 16777216 bytes"),
        ]
        for name, op, req, want in tests:
            with self.subTest(name):
                code, out, _ = call(Full(), op, req)
                self.assertEqual(code, EXIT_ERROR, out)
                e = error_of(out)
                self.assertEqual(e["code"], INVALID_CONFIG)
                self.assertIn(want, e["message"])

    def test_envelope_may_be_left_out(self):
        for req in ('{"secrets":{"api_token":"good"}}', '{"apiVersion":"","kind":"","secrets":{"api_token":"good"}}'):
            with self.subTest(req):
                code, out, _ = call(Fake(), OP_LIST, req)
                self.assertEqual(code, EXIT_OK, out)
        code, out, _ = call(Fake(), OP_LIST, "null")
        self.assertEqual(error_of(out)["code"], UNAUTHENTICATED)

    def test_encode_request_round_trip(self):
        b = encode_request(OP_LIST, ListRequest(account=Account("work"), secrets={"api_token": "good"}))
        self.assertIn(b'"kind":"ListRequest"', b)
        self.assertIn(b'"apiVersion":"provider/v1alpha1"', b)
        code, out, _ = call(Fake(), OP_LIST, b)
        self.assertEqual(code, EXIT_OK, out)
        self.assertEqual(encode_request(OP_DESCRIBE), b'{"apiVersion":"provider/v1alpha1","kind":"DescribeRequest"}')
        self.assertEqual(
            encode_request(OP_CREDENTIAL, CredentialRequest(account=Account("demo"), cluster_id="dev", secrets={"b": "2", "a": "1"})),
            b'{"apiVersion":"provider/v1alpha1","kind":"CredentialRequest","account":{"id":"demo"},"clusterID":"dev","secrets":{"a":"1","b":"2"}}',
        )
        with self.assertRaises(TypeError):
            encode_request(OP_LIST, CredentialRequest())
        with self.assertRaises(ValueError):
            encode_request("bogus")

    def test_decode_checks_the_envelope(self):
        tests = [
            ("not JSON", b"nope", "response is not JSON"),
            ("wrong kind", b'{"apiVersion":"provider/v1alpha1","kind":"ListResponse"}', 'expected kind CredentialResponse, got "ListResponse"'),
            ("no version", b'{"kind":"CredentialResponse"}', 'unsupported apiVersion ""'),
            ("invalid credential", b'{"apiVersion":"provider/v1alpha1","kind":"CredentialResponse","credential":{"type":"oidc"}}', "invalid credential"),
            ("malformed", b'{"apiVersion":"provider/v1alpha1","kind":"CredentialResponse","credential":{"type":1}}', "malformed CredentialResponse"),
        ]
        for name, data, want in tests:
            with self.subTest(name):
                with self.assertRaises(ProviderError) as cm:
                    decode_credential(data)
                self.assertEqual(cm.exception.code, INTERNAL)
                self.assertIn(want, cm.exception.message)
        with self.assertRaises(ProviderError) as cm:
            decode_list('{"kind":"Error","code":"NotFound","message":"gone"}')
        self.assertEqual(code_of(cm.exception), "NotFound")

    def test_cancel_while_reading_request(self):
        code, out, _ = call(Fake(), OP_LIST, Cancelling())
        self.assertEqual(code, EXIT_ERROR)
        self.assertEqual(error_of(out)["code"], UNAVAILABLE)
        self.assertEqual(error_of(out)["message"], "cancelled while reading the request: received SIGTERM")

    def test_cancel_while_running(self):
        class Slow(Fake):
            def list(self, request):
                raise Canceled("received SIGTERM")

        code, out, _ = call(Slow(), OP_LIST, "{}")
        self.assertEqual((code, error_of(out)["code"], error_of(out)["message"]), (EXIT_ERROR, UNAVAILABLE, "list cancelled: received SIGTERM"))

    def test_text_streams(self):
        out, err = io.StringIO(), io.StringIO()
        code = run(Fake(), [OP_LIST], io.StringIO('{"secrets":{"api_token":"good"}}'), out, err)
        self.assertEqual(code, EXIT_OK, out.getvalue())
        self.assertEqual(len(decode_list(out.getvalue())), 1)

    def test_write_failure(self):
        class Closed(io.RawIOBase):
            def writable(self):
                return True

            def write(self, b):
                raise BrokenPipeError("broken pipe")

        err = io.StringIO()
        code = run(Fake(), [OP_DESCRIBE], io.BytesIO(), Closed(), err)
        self.assertEqual(code, EXIT_ERROR)
        self.assertIn("writing response: broken pipe", err.getvalue())

    def test_public_api(self):
        missing = [name for name in tug_provider.__all__ if not hasattr(tug_provider, name)]
        self.assertEqual(missing, [])


def plugin(*args, **kwargs):
    env = dict(os.environ, PYTHONPATH=SDK, PYTHONDONTWRITEBYTECODE="1")
    return subprocess.Popen(
        [sys.executable, FIXTURE] + list(args),
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        **kwargs
    )


class MainTest(unittest.TestCase):
    def test_print_goes_to_stderr(self):
        p = plugin(OP_LIST)
        out, err = p.communicate(b"{}", timeout=30)
        self.assertEqual(p.returncode, EXIT_OK, err)
        self.assertEqual(len(decode_list(out)), 1)
        self.assertIn(b"stray output from list", err)

    def test_exit_codes(self):
        tests = [((OP_DESCRIBE,), b"", EXIT_OK), ((OP_LIST,), b"", EXIT_ERROR), (("bogus",), b"", EXIT_USAGE), ((), b"", EXIT_USAGE)]
        for args, stdin, want in tests:
            with self.subTest(args):
                p = plugin(*args)
                _, err = p.communicate(stdin, timeout=30)
                self.assertEqual(p.returncode, want, err)

    @unittest.skipIf(sys.platform == "win32", "Windows has no SIGTERM; tug kills plugins there")
    def test_sigterm_while_reading_request(self):
        p = plugin(OP_LIST)
        try:
            time.sleep(0.5)  # let it start reading the request
            start = time.monotonic()
            p.send_signal(signal.SIGTERM)
            out, err = p.communicate(timeout=2)
        finally:
            p.kill()
        self.assertLess(time.monotonic() - start, 2)
        self.assertEqual(p.returncode, EXIT_ERROR, err)
        self.assertEqual(error_of(out)["message"], "cancelled while reading the request: received SIGTERM")

    @unittest.skipIf(sys.platform == "win32", "Windows has no SIGTERM; tug kills plugins there")
    def test_sigterm_while_running(self):
        with plugin(OP_LIST) as p:
            try:
                p.stdin.write(b'{"account":{"id":"slow"}}')
                p.stdin.close()
                time.sleep(0.5)  # let it reach the sleep
                p.send_signal(signal.SIGTERM)
                p.wait(timeout=2)
            finally:
                p.kill()
            out, err = p.stdout.read(), p.stderr.read()
        self.assertEqual(p.returncode, EXIT_ERROR, err)
        self.assertEqual(error_of(out)["message"], "list cancelled: received SIGTERM")
        self.assertIn(b"cleaning up", err)


if __name__ == "__main__":
    unittest.main()
