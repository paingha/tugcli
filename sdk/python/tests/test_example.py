"""Tests that run the example plugin the way tug does: one process per
operation."""

import os
import subprocess
import sys
import unittest

from tug_provider import (
    INVALID_CONFIG,
    OP_ACCOUNTS,
    OP_CREDENTIAL,
    OP_DESCRIBE,
    OP_LIST,
    UNAUTHENTICATED,
    Account,
    CredentialRequest,
    ListRequest,
    ProviderError,
    decode_accounts,
    decode_credential,
    decode_describe,
    decode_list,
    encode_request,
)

SDK = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXAMPLE = os.path.join(SDK, "examples", "tug-provider-pyexample", "tug-provider-pyexample")
DEMO = Account("demo", config={"clusters": "dev=https://127.0.0.1:6443, staging=https://127.0.0.1:7443"})


def call(op, payload=None, clusters=None):
    """Runs the example for op and returns its exit code and stdout.
    clusters sets PYEXAMPLE_CLUSTERS."""
    stdin = b"" if op in (OP_DESCRIBE, OP_ACCOUNTS) else encode_request(op, payload)
    environ = dict(os.environ, PYTHONPATH=SDK, PYTHONDONTWRITEBYTECODE="1")
    environ.pop("PYEXAMPLE_CLUSTERS", None)
    if clusters is not None:
        environ["PYEXAMPLE_CLUSTERS"] = clusters
    out = subprocess.run([sys.executable, EXAMPLE, op], input=stdin, capture_output=True, env=environ, timeout=30)
    return out.returncode, out.stdout


class ExampleTest(unittest.TestCase):
    def test_describe(self):
        code, out = call(OP_DESCRIBE)
        self.assertEqual(code, 0)
        d = decode_describe(out)
        self.assertEqual((d.name, d.capabilities), ("pyexample", ["accounts", "list", "credential", "serve"]))

    def test_list(self):
        code, out = call(OP_LIST, ListRequest(account=DEMO, secrets={"api_token": "ex_x"}))
        self.assertEqual(code, 0, out)
        self.assertEqual([(c.id, c.endpoints.public) for c in decode_list(out)], [("dev", "https://127.0.0.1:6443"), ("staging", "https://127.0.0.1:7443")])

    def test_errors(self):
        tests = [
            ("bad token", ListRequest(account=DEMO, secrets={"api_token": "nope"}), UNAUTHENTICATED),
            ("no token", ListRequest(account=DEMO), UNAUTHENTICATED),
            ("no clusters setting", ListRequest(account=Account("demo"), secrets={"api_token": "ex_x"}), INVALID_CONFIG),
            ("bad entry", ListRequest(account=Account("demo", config={"clusters": "dev"}), secrets={"api_token": "ex_x"}), INVALID_CONFIG),
        ]
        for name, req, want in tests:
            with self.subTest(name):
                code, out = call(OP_LIST, req)
                self.assertEqual(code, 1)
                with self.assertRaises(ProviderError) as cm:
                    decode_list(out)
                self.assertEqual(cm.exception.code, want)

    def test_credential(self):
        code, out = call(OP_CREDENTIAL, CredentialRequest(account=DEMO, cluster_id="dev", secrets={"api_token": "ex_x"}))
        self.assertEqual(code, 0, out)
        c = decode_credential(out)
        self.assertEqual((c.type, c.token, c.expires_at), ("token", "ex_x", None))

    def test_accounts(self):
        code, out = call(OP_ACCOUNTS)
        self.assertEqual((code, decode_accounts(out)), (0, []))
        code, out = call(OP_ACCOUNTS, clusters="dev=https://127.0.0.1:6443")
        self.assertEqual(code, 0, out)
        self.assertEqual([a.config for a in decode_accounts(out)], [{"clusters": "dev=https://127.0.0.1:6443"}])


if __name__ == "__main__":
    unittest.main()
