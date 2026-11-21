"""A plugin whose answers wire_test.go compares, byte for byte, with the Go
SDK's answers from the same fixtures.

wirePlugin in wire_test.go is the Go copy of Wire below: change both
together. `wire.py encode` instead prints the requests encode_request makes
for the fixture payloads, one per line, for the same comparison.
"""

import sys
from datetime import datetime, timedelta, timezone

from tug_provider import (
    NO_EXPIRY,
    NOT_FOUND,
    OP_ACCOUNTS,
    OP_CHECK_ACCESS,
    OP_CREDENTIAL,
    OP_DESCRIBE,
    OP_LIST,
    OP_ROTATE,
    RATE_LIMITED,
    SEVERITY_ERROR,
    SEVERITY_INFO,
    SEVERITY_WARNING,
    UNAUTHENTICATED,
    AccessRequest,
    Account,
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
    client_cert,
    delegate,
    encode_request,
    main,
    oidc,
    token,
)

EXPIRY = datetime(2030, 1, 2, 3, 4, 5, 123456, tzinfo=timezone.utc)
CA = b"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"


def full_cluster():
    return Cluster(
        id="c-1",
        name="prod-eu",
        region="eu-west",
        version="1.33",
        endpoints=Endpoints(public="https://a.example:443", private="https://10.0.0.1:6443"),
        ca_data=CA,
        tls_server_name="kubernetes",
        insecure_skip_tls_verify=True,
        proxy_url="socks5://127.0.0.1:1080",
        default_namespace="payments",
        tunnel="ssh://admin@bastion.example.com",
        labels={"team": "platform", "env": "prod", "gpu": "true"},
        credential=token("inline", EXPIRY),
    )


class Wire(Provider):
    def info(self):
        return Info(
            name="wire",
            version="1.0.0",
            description="Wire <format> & fixtures",
            secrets=[SecretSpec("api_token", prompt="API token"), SecretSpec("region_key", optional=True)],
        )

    def accounts(self):
        return [
            Account("work", description="café profile", config={"profile": "work", "nested": {"b": 1, "a": [True, None, "x"]}, "count": 3}),
            Account("home"),
        ]

    def list(self, request):
        api_token = request.secrets.get("api_token", "")
        if api_token == "empty":
            return None
        if api_token == "limited":
            raise ProviderError(RATE_LIMITED, "GET /v1/clusters: slow <down> & wait", hint="wait   a minute", retry_after_seconds=30)
        if api_token != "good":
            raise ProviderError(UNAUTHENTICATED, "no api_token").with_hint("set one")
        return [
            full_cluster(),
            Cluster(id="c-2", name="dev", endpoints=Endpoints(public="https://b.example")),
            Cluster(
                id="c-3",
                name="ünïcode-東京",
                endpoints=Endpoints(private="https://c.example"),
                labels={"note": 'a<b>&c  \x01\t"\\', "account": request.account.id},
            ),
        ]

    def credential(self, request):
        cid = request.cluster_id
        if cid == "token":
            return token("tok-" + request.account.id, EXPIRY)
        if cid == "token-local":
            return token("tok", datetime(2030, 1, 2, 3, 4, 5, tzinfo=timezone(timedelta(hours=1))))
        if cid == "noexpiry":
            return token("tok", NO_EXPIRY)
        if cid == "cert":
            return client_cert(CA, b"KEY", EXPIRY)
        if cid == "oidc":
            return oidc("https://issuer.example", "tug", "openid", "offline_access", client_secret="s3cret")
        if cid == "delegate":
            return delegate(
                "aws",
                "eks",
                "get-token",
                "--cluster-name",
                cid,
                env={"B": "2", "A": "1"},
                api_version="client.authentication.k8s.io/v1",
                interactive_mode="IfAvailable",
                install_hint="install <aws> & co",
            )
        if cid == "invalid":
            return Credential(type="token")
        raise ProviderError(NOT_FOUND, 'no cluster "%s"' % cid)

    def rotate(self, request):
        return token("rotated-%s-%s" % (request.cluster_id, request.secrets.get("api_token", "")), EXPIRY)

    def check_access(self, request):
        c = request.cluster
        findings = [
            Finding(SEVERITY_ERROR, "allow list lacks " + request.egress_ip, fix="add " + request.egress_ip + "/32"),
            Finding(SEVERITY_INFO, "%s in %s has %d bytes of CA at %s" % (c.name, request.account.id, len(c.ca_data), c.endpoints.public)),
        ]
        if c.credential is not None:
            expires = c.credential.to_dict().get("expiresAt", "never")
            findings.append(Finding(SEVERITY_WARNING, "inline %s credential expires %s" % (c.credential.type, expires)))
        return findings


def encode_all():
    secrets = {"z": "1", "a": "<&>"}
    requests = [
        (OP_DESCRIBE, None),
        (OP_ACCOUNTS, None),
        (OP_LIST, ListRequest(account=Account("work", description="d", config={"b": "2", "a": {"z": [1, "x"]}}), secrets=secrets)),
        (OP_LIST, ListRequest()),
        (OP_CREDENTIAL, CredentialRequest(account=Account("demo"), cluster_id="dev", secrets=secrets)),
        (OP_ROTATE, CredentialRequest(account=Account("demo"), cluster_id="dev")),
        (OP_CHECK_ACCESS, AccessRequest(account=Account("w"), cluster=full_cluster(), egress_ip="198.51.100.9", secrets=secrets)),
        (OP_CHECK_ACCESS, AccessRequest()),
    ]
    for op, payload in requests:
        sys.stdout.buffer.write(encode_request(op, payload) + b"\n")


if __name__ == "__main__":
    if sys.argv[1:] == ["encode"]:
        encode_all()
        sys.exit(0)
    sys.exit(main(Wire()))
