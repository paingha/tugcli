"""Tests for the payload types and their JSON form.

The expected JSON was written by the Go SDK's json.Marshal, so these tests
pin the wire format to the Go one.
"""

import json
import unittest
from datetime import datetime, timedelta, timezone

from tug_provider import (
    NO_EXPIRY,
    SEVERITY_ERROR,
    AccessRequest,
    Account,
    Cluster,
    Credential,
    CredentialRequest,
    Description,
    Endpoints,
    Finding,
    Info,
    ListRequest,
    SecretSpec,
    client_cert,
    delegate,
    oidc,
    token,
)
from tug_provider._wire import dumps, format_time, parse_time

EXPIRY = datetime(2030, 1, 2, 3, 4, 5, 123456, tzinfo=timezone.utc)
CET = timezone(timedelta(hours=1))


def encode(value):
    return dumps(value.to_dict()).decode("utf-8")


class WireFormatTest(unittest.TestCase):
    def test_matches_go(self):
        tests = [
            (
                "full cluster",
                Cluster(
                    id="c-1",
                    name="prod-eu",
                    region="eu-west",
                    version="1.33",
                    endpoints=Endpoints(public="https://a.example:443", private="https://10.0.0.1:6443"),
                    ca_data=b"-----BEGIN CERTIFICATE-----\n",
                    tls_server_name="kubernetes",
                    insecure_skip_tls_verify=True,
                    proxy_url="socks5://127.0.0.1:1080",
                    default_namespace="payments",
                    tunnel="ssh://admin@bastion.example.com",
                    labels={"team": "platform", "env": "prod", "note": "a<b>&c "},
                    credential=token("inline", EXPIRY),
                ),
                '{"id":"c-1","name":"prod-eu","region":"eu-west","version":"1.33",'
                '"endpoints":{"public":"https://a.example:443","private":"https://10.0.0.1:6443"},'
                '"caData":"LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCg==","tlsServerName":"kubernetes",'
                '"insecureSkipTLSVerify":true,"proxyURL":"socks5://127.0.0.1:1080","defaultNamespace":"payments",'
                '"tunnel":"ssh://admin@bastion.example.com",'
                '"labels":{"env":"prod","note":"a\\u003cb\\u003e\\u0026c\\u2028","team":"platform"},'
                '"credential":{"type":"token","token":"inline","expiresAt":"2030-01-02T03:04:05.123456Z"}}',
            ),
            ("minimal cluster", Cluster(id="c-2", name="dev"), '{"id":"c-2","name":"dev","endpoints":{}}'),
            (
                "token with a local expiry",
                token("tok", datetime(2030, 1, 2, 3, 4, 5, tzinfo=CET)),
                '{"type":"token","token":"tok","expiresAt":"2030-01-02T02:04:05Z"}',
            ),
            ("token without expiry", token("tok", NO_EXPIRY), '{"type":"token","token":"tok"}'),
            (
                "client cert",
                client_cert(b"CERT", "KEY", EXPIRY),
                '{"type":"clientCert","certData":"Q0VSVA==","keyData":"S0VZ","expiresAt":"2030-01-02T03:04:05.123456Z"}',
            ),
            (
                "oidc",
                oidc("https://issuer.example", "tug", "openid", "offline_access", client_secret="s3cret"),
                '{"type":"oidc","issuer":"https://issuer.example","clientID":"tug","clientSecret":"s3cret",'
                '"scopes":["openid","offline_access"]}',
            ),
            (
                "delegate",
                delegate(
                    "aws",
                    "eks",
                    "get-token",
                    env={"B": "2", "A": "1"},
                    api_version="client.authentication.k8s.io/v1",
                    interactive_mode="IfAvailable",
                    install_hint="install the AWS CLI",
                ),
                '{"type":"delegate","exec":{"command":"aws","args":["eks","get-token"],"env":{"A":"1","B":"2"},'
                '"apiVersion":"client.authentication.k8s.io/v1","interactiveMode":"IfAvailable",'
                '"installHint":"install the AWS CLI"}}',
            ),
            (
                "delegate with cluster info",
                delegate("secretreader-plugin", provide_cluster_info=True, cluster_config={"z": [1], "clusterName": "member-1"}),
                '{"type":"delegate","exec":{"command":"secretreader-plugin","provideClusterInfo":true,'
                '"clusterConfig":{"clusterName":"member-1","z":[1]}}}',
            ),
            (
                "account with nested config",
                Account(id="work", description="café", config={"z": 1, "a": {"y": True, "b": [None, "x"]}}),
                '{"id":"work","description":"café","config":{"a":{"b":[null,"x"],"y":true},"z":1}}',
            ),
            ("finding", Finding(SEVERITY_ERROR, "m", fix="f"), '{"severity":"error","message":"m","fix":"f"}'),
        ]
        for name, value, want in tests:
            with self.subTest(name):
                self.assertEqual(encode(value), want)

    def test_round_trip(self):
        cluster = Cluster(
            id="c-1",
            name="prod",
            endpoints=Endpoints(public="https://a"),
            ca_data=b"\x00\xffPEM",
            labels={"env": "prod"},
            credential=client_cert(b"C", b"K", EXPIRY),
        )
        self.assertEqual(Cluster.from_dict(json.loads(encode(cluster))), cluster)
        for cred in (
            token("t", EXPIRY),
            oidc("https://i", "c", "openid"),
            delegate("x", "a", install_hint="h"),
            delegate("x", provide_cluster_info=True, cluster_config={"clusterName": "m", "n": [None, 1.5]}),
        ):
            with self.subTest(cred.type):
                self.assertEqual(Credential.from_dict(json.loads(encode(cred))), cred)
        req = AccessRequest(account=Account("w", config={"k": "v"}), cluster=cluster, egress_ip="198.51.100.9", secrets={"a": "1"})
        self.assertEqual(AccessRequest.from_dict(json.loads(encode(req))), req)

    def test_requests_without_fields_have_zero_values(self):
        self.assertEqual(ListRequest.from_dict({}), ListRequest())
        self.assertEqual(ListRequest.from_dict(None), ListRequest())
        self.assertEqual(CredentialRequest.from_dict({"account": None, "secrets": {"a": None}}), CredentialRequest(secrets={"a": ""}))
        self.assertEqual(AccessRequest.from_dict({"cluster": {}}).cluster, Cluster(id="", name=""))

    def test_unknown_fields_are_ignored(self):
        req = CredentialRequest.from_dict({"clusterID": "c", "future": [1, 2], "account": {"id": "a", "new": True}})
        self.assertEqual(req, CredentialRequest(account=Account("a"), cluster_id="c"))

    def test_wrong_types_are_refused(self):
        tests = [
            (ListRequest, {"account": "work"}, "request.account is a string, want an object"),
            (ListRequest, {"account": {"id": 7}}, "request.account.id is a number, want a string"),
            (ListRequest, {"secrets": {"api_token": 1}}, "request.secrets.api_token is a number, want a string"),
            (ListRequest, {"secrets": ["x"]}, "request.secrets is an array, want an object of strings"),
            (CredentialRequest, {"clusterID": True}, "request.clusterID is a boolean, want a string"),
            (AccessRequest, {"cluster": {"caData": "not base64!"}}, "request.cluster.caData is not base64"),
            (AccessRequest, {"cluster": {"credential": {"expiresAt": "tomorrow"}}}, "request.cluster.credential.expiresAt"),
            (AccessRequest, {"cluster": {"insecureSkipTLSVerify": "yes"}}, "is a string, want a boolean"),
            (AccessRequest, {"cluster": {"credential": {"scopes": [1]}}}, "request.cluster.credential.scopes[0] is a number"),
        ]
        for cls, value, want in tests:
            with self.subTest(want):
                with self.assertRaises(ValueError) as cm:
                    cls.from_dict(value)
                self.assertIn(want, str(cm.exception))

    def test_describe_ignores_unknown_capabilities(self):
        d = Description.from_dict({"name": "x", "capabilities": ["list", "teleport", "rotate"]})
        self.assertEqual(d.capabilities, ["list", "rotate"])


class CredentialTest(unittest.TestCase):
    def test_validate(self):
        tests = [
            ("token", token("t", NO_EXPIRY), ""),
            ("token without token", Credential(type="token"), "no token"),
            ("client cert", client_cert(b"C", b"K", NO_EXPIRY), ""),
            ("client cert without key", client_cert(b"C", b"", NO_EXPIRY), "certData and keyData"),
            ("client cert without cert", client_cert(None, b"K", NO_EXPIRY), "certData and keyData"),
            ("oidc", oidc("https://issuer", "tug", "openid"), ""),
            ("oidc without client ID", oidc("https://issuer", ""), "issuer and clientID"),
            ("oidc without issuer", oidc("", "tug"), "issuer and clientID"),
            ("delegate", delegate("aws", "eks", "get-token"), ""),
            ("delegate without exec", Credential(type="delegate"), "exec.command"),
            ("delegate without command", delegate(""), "exec.command"),
            ("delegate with cluster info", delegate("cp-creds", provide_cluster_info=True), ""),
            ("delegate with cluster info and config", delegate("cp-creds", provide_cluster_info=True, cluster_config={"a": 1}), ""),
            ("delegate with cluster config only", delegate("cp-creds", cluster_config={"a": 1}), "without exec.provideClusterInfo"),
            ("unknown type", Credential(type="password"), '"password"'),
            ("no type", Credential(type="", token="t"), "unknown credential type"),
        ]
        for name, cred, want in tests:
            with self.subTest(name):
                if not want:
                    cred.validate()
                    continue
                with self.assertRaises(ValueError) as cm:
                    cred.validate()
                self.assertIn(want, str(cm.exception))

    def test_expired(self):
        now = datetime(2026, 11, 20, 12, 0, 0, tzinfo=timezone.utc)
        skew = timedelta(minutes=1)
        tests = [
            ("no expiry", NO_EXPIRY, False),
            ("well before expiry", now + timedelta(hours=1), False),
            ("just outside the skew", now + skew + timedelta(seconds=1), False),
            ("exactly at the skew", now + skew, True),
            ("within the skew", now + timedelta(seconds=30), True),
            ("already expired", now - timedelta(hours=1), True),
        ]
        for name, expires_at, want in tests:
            with self.subTest(name):
                self.assertEqual(token("t", expires_at).expired(now, skew), want)
        self.assertTrue(token("t", datetime(2000, 1, 1, tzinfo=timezone.utc)).expired())

    def test_constructors_normalise_expiry(self):
        local = datetime(2026, 11, 20, 13, 0, 0, tzinfo=CET)
        c = client_cert(b"C", b"K", local)
        self.assertEqual(c.expires_at.utcoffset(), timedelta(0))
        self.assertEqual(c.expires_at, local)

    def test_constructors_refuse_naive_times(self):
        with self.assertRaises(ValueError):
            token("t", datetime(2030, 1, 1))
        with self.assertRaises(TypeError):
            token("t", 1893456000)


class TimeTest(unittest.TestCase):
    def test_format(self):
        tests = [
            (datetime(2030, 1, 2, 3, 4, 5, tzinfo=timezone.utc), "2030-01-02T03:04:05Z"),
            (datetime(2030, 1, 2, 3, 4, 5, 120000, tzinfo=timezone.utc), "2030-01-02T03:04:05.12Z"),
            (datetime(2030, 1, 2, 3, 4, 5, tzinfo=CET), "2030-01-02T03:04:05+01:00"),
            (datetime(2030, 1, 2, 3, 4, 5, tzinfo=timezone(-timedelta(hours=5, minutes=30))), "2030-01-02T03:04:05-05:30"),
            (datetime(999, 1, 2, tzinfo=timezone.utc), "0999-01-02T00:00:00Z"),
        ]
        for t, want in tests:
            with self.subTest(want):
                self.assertEqual(format_time(t), want)
        with self.assertRaises(ValueError):
            format_time(datetime(2030, 1, 1))

    def test_parse(self):
        tests = [
            ("2030-01-02T03:04:05Z", datetime(2030, 1, 2, 3, 4, 5, tzinfo=timezone.utc)),
            ("2030-01-02T03:04:05.123456789Z", datetime(2030, 1, 2, 3, 4, 5, 123456, tzinfo=timezone.utc)),
            ("2030-01-02T03:04:05.5+01:00", datetime(2030, 1, 2, 3, 4, 5, 500000, tzinfo=CET)),
        ]
        for s, want in tests:
            with self.subTest(s):
                self.assertEqual(parse_time(s), want)
        for bad in ("2030-01-02", "2030-01-02 03:04:05Z", "2030-13-02T03:04:05Z", "2030-01-02T03:04:05"):
            with self.subTest(bad):
                with self.assertRaises(ValueError):
                    parse_time(bad)


class InfoTest(unittest.TestCase):
    def test_unknown_capability_is_refused(self):
        with self.assertRaises(ValueError):
            Info(name="x", capabilities=["teleport"])

    def test_account_config_string(self):
        a = Account("work", config={"org": "acme", "count": 3})
        self.assertEqual((a.config_string("org"), a.config_string("count"), a.config_string("missing")), ("acme", "", ""))
        self.assertEqual(Account("x", config=None).config_string("org"), "")

    def test_secret_spec(self):
        self.assertEqual(encode(SecretSpec("api_token")), '{"name":"api_token"}')
        self.assertEqual(encode(SecretSpec("k", prompt="Key", optional=True)), '{"name":"k","prompt":"Key","optional":true}')


if __name__ == "__main__":
    unittest.main()
