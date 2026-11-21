"""The payload types of protocol provider/v1alpha1 and their JSON form.

Each type has to_dict, which returns exactly the object the Go SDK writes
(camelCase names, empty fields left out, base64 bytes, RFC 3339 times), and
from_dict, which reads one as the Go SDK does and raises ValueError for a
field of the wrong type.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Optional

from ._wire import Fields, encode_bytes, format_time, quote, sorted_map

API_VERSION = "provider/v1alpha1"
"""The protocol version this SDK speaks."""

# Capability names. Describe lists them in this order.
CAP_ACCOUNTS = "accounts"
CAP_LIST = "list"
CAP_CREDENTIAL = "credential"
CAP_ROTATE = "rotate"
CAP_CHECK_ACCESS = "checkAccess"
CAP_SERVE = "serve"
CAPABILITY_NAMES = (CAP_ACCOUNTS, CAP_LIST, CAP_CREDENTIAL, CAP_ROTATE, CAP_CHECK_ACCESS, CAP_SERVE)
"""Every capability name, in the order describe lists them."""

# Credential shapes.
CREDENTIAL_TOKEN = "token"
CREDENTIAL_CLIENT_CERT = "clientCert"
CREDENTIAL_OIDC = "oidc"
CREDENTIAL_DELEGATE = "delegate"

# Severities of an access finding, from least to most serious.
SEVERITY_INFO = "info"
SEVERITY_WARNING = "warning"
SEVERITY_ERROR = "error"

NO_EXPIRY = None
"""Marks a credential that does not expire."""


def sort_capabilities(names: Any) -> list[str]:
    """Returns the known capability names among names, in describe's order.
    Unknown names are dropped, so newer plugins keep working with older
    hosts."""
    have = set(names)
    return [c for c in CAPABILITY_NAMES if c in have]


@dataclass
class SecretSpec:
    """Declares a secret the provider needs. tug prompts for it once per
    account and keeps it in the OS keychain."""

    name: str
    prompt: str = ""
    optional: bool = False

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the spec."""
        d: dict[str, Any] = {"name": self.name}
        if self.prompt:
            d["prompt"] = self.prompt
        if self.optional:
            d["optional"] = True
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "secret") -> SecretSpec:
        """Reads a spec from its JSON object."""
        f = Fields(value, where)
        return cls(name=f.string("name"), prompt=f.string("prompt"), optional=f.boolean("optional"))


@dataclass
class Info:
    """Describes a provider. capabilities lists any capabilities beyond the
    ones main works out for itself: list and credential always, and each
    optional operation the provider overrides."""

    name: str
    version: str = ""
    description: str = ""
    capabilities: list[str] = field(default_factory=list)
    secrets: list[SecretSpec] = field(default_factory=list)

    def __post_init__(self) -> None:
        for name in self.capabilities:
            if name not in CAPABILITY_NAMES:
                raise ValueError("unknown capability %s" % quote(name))


@dataclass
class Description:
    """The answer to describe, as a host reads it."""

    name: str
    version: str = ""
    description: str = ""
    protocols: list[str] = field(default_factory=list)
    capabilities: list[str] = field(default_factory=list)
    secrets: list[SecretSpec] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the description."""
        d: dict[str, Any] = {"name": self.name}
        if self.version:
            d["version"] = self.version
        if self.description:
            d["description"] = self.description
        d["protocols"] = list(self.protocols)
        d["capabilities"] = sort_capabilities(self.capabilities)
        if self.secrets:
            d["secrets"] = [s.to_dict() for s in self.secrets]
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "description") -> Description:
        """Reads a description from its JSON object. Unknown capability names
        are ignored."""
        f = Fields(value, where)
        return cls(
            name=f.string("name"),
            version=f.string("version"),
            description=f.string("description"),
            protocols=f.string_list("protocols"),
            capabilities=sort_capabilities(f.string_list("capabilities")),
            secrets=[SecretSpec.from_dict(s, "%s[%d]" % (f.path("secrets"), i)) for i, s in enumerate(f.array("secrets"))],
        )


@dataclass
class Account:
    """One set of provider settings, for example an AWS profile or a
    CoreWeave organisation. config holds provider-specific settings that the
    manifest's accountConfig schema describes."""

    id: str
    description: str = ""
    config: dict[str, Any] = field(default_factory=dict)

    def config_string(self, key: str) -> str:
        """Returns a string setting from config, or "" when it is missing or
        not a string. Settings from `tug import --set` are always strings."""
        v = (self.config or {}).get(key)
        return v if isinstance(v, str) else ""

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the account."""
        d: dict[str, Any] = {"id": self.id}
        if self.description:
            d["description"] = self.description
        if self.config:
            d["config"] = sorted_map(self.config)
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "account") -> Account:
        """Reads an account from its JSON object."""
        f = Fields(value, where)
        return cls(id=f.string("id"), description=f.string("description"), config=f.any_map("config"))


@dataclass
class Endpoints:
    """The API server URLs of a cluster."""

    public: str = ""
    private: str = ""

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the endpoints."""
        d: dict[str, Any] = {}
        if self.public:
            d["public"] = self.public
        if self.private:
            d["private"] = self.private
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "endpoints") -> Endpoints:
        """Reads endpoints from their JSON object."""
        f = Fields(value, where)
        return cls(public=f.string("public"), private=f.string("private"))


@dataclass
class ExecConfig:
    """A command that prints a client.authentication.k8s.io ExecCredential,
    for example a vendor's own kubectl exec plugin.

    provide_cluster_info asks tug to tell the command which cluster it
    authenticates to, as kubectl does for a kubeconfig exec plugin that sets
    provideClusterInfo: KUBERNETES_EXEC_INFO then has a spec.cluster with the
    cluster's server, CA data, TLS server name, insecure flag and proxy URL.
    cluster_config, any JSON value or None, is passed as spec.cluster.config,
    which kubectl fills from the cluster's client.authentication.k8s.io/exec
    kubeconfig extension. It needs provide_cluster_info and is not meant for
    secrets."""

    command: str
    args: list[str] = field(default_factory=list)
    env: dict[str, str] = field(default_factory=dict)
    api_version: str = ""
    interactive_mode: str = ""
    install_hint: str = ""
    provide_cluster_info: bool = False
    cluster_config: Any = None

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the command."""
        d: dict[str, Any] = {"command": self.command}
        if self.args:
            d["args"] = list(self.args)
        if self.env:
            d["env"] = sorted_map(self.env)
        if self.api_version:
            d["apiVersion"] = self.api_version
        if self.interactive_mode:
            d["interactiveMode"] = self.interactive_mode
        if self.install_hint:
            d["installHint"] = self.install_hint
        if self.provide_cluster_info:
            d["provideClusterInfo"] = True
        if self.cluster_config is not None:
            d["clusterConfig"] = sorted_map(self.cluster_config)
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "exec") -> ExecConfig:
        """Reads a command from its JSON object."""
        f = Fields(value, where)
        return cls(
            command=f.string("command"),
            args=f.string_list("args"),
            env=f.string_map("env"),
            api_version=f.string("apiVersion"),
            interactive_mode=f.string("interactiveMode"),
            install_hint=f.string("installHint"),
            provide_cluster_info=f.boolean("provideClusterInfo"),
            cluster_config=f.raw("clusterConfig"),
        )


@dataclass
class Credential:
    """What a provider returns for one cluster. Exactly one shape is filled,
    as named by type: use token, client_cert, oidc or delegate to make one.
    cert_data and key_data are PEM. expires_at is when tug must ask again;
    None means the credential does not expire."""

    type: str
    token: str = ""
    cert_data: bytes = b""
    key_data: bytes = b""
    issuer: str = ""
    client_id: str = ""
    client_secret: str = ""
    scopes: list[str] = field(default_factory=list)
    exec: Optional[ExecConfig] = None
    expires_at: Optional[datetime] = None

    def validate(self) -> None:
        """Raises ValueError when the credential lacks a field its type
        needs."""
        if self.type == CREDENTIAL_TOKEN:
            if not self.token:
                raise ValueError("token credential has no token")
        elif self.type == CREDENTIAL_CLIENT_CERT:
            if not self.cert_data or not self.key_data:
                raise ValueError("clientCert credential needs certData and keyData")
        elif self.type == CREDENTIAL_OIDC:
            if not self.issuer or not self.client_id:
                raise ValueError("oidc credential needs issuer and clientID")
        elif self.type == CREDENTIAL_DELEGATE:
            if self.exec is None or not self.exec.command:
                raise ValueError("delegate credential needs exec.command")
            if self.exec.cluster_config is not None and not self.exec.provide_cluster_info:
                # kubectl passes the extension only with provideClusterInfo,
                # so the command would never see it.
                raise ValueError("delegate credential sets exec.clusterConfig without exec.provideClusterInfo")
        else:
            raise ValueError("unknown credential type %s" % quote(str(self.type)))

    def expired(self, now: Optional[datetime] = None, skew: timedelta = timedelta(0)) -> bool:
        """Reports whether the credential expires within skew of now, which
        defaults to the current time."""
        if self.expires_at is None:
            return False
        if now is None:
            now = datetime.now(timezone.utc)
        return not now + skew < self.expires_at

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the credential."""
        d: dict[str, Any] = {"type": self.type}
        if self.token:
            d["token"] = self.token
        if self.cert_data:
            d["certData"] = encode_bytes(self.cert_data)
        if self.key_data:
            d["keyData"] = encode_bytes(self.key_data)
        if self.issuer:
            d["issuer"] = self.issuer
        if self.client_id:
            d["clientID"] = self.client_id
        if self.client_secret:
            d["clientSecret"] = self.client_secret
        if self.scopes:
            d["scopes"] = list(self.scopes)
        if self.exec is not None:
            d["exec"] = self.exec.to_dict()
        if self.expires_at is not None:
            d["expiresAt"] = format_time(self.expires_at)
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "credential") -> Credential:
        """Reads a credential from its JSON object. It is not validated."""
        f = Fields(value, where)
        exec_config = f.raw("exec")
        return cls(
            type=f.string("type"),
            token=f.string("token"),
            cert_data=f.data("certData"),
            key_data=f.data("keyData"),
            issuer=f.string("issuer"),
            client_id=f.string("clientID"),
            client_secret=f.string("clientSecret"),
            scopes=f.string_list("scopes"),
            exec=None if exec_config is None else ExecConfig.from_dict(exec_config, f.path("exec")),
            expires_at=f.time("expiresAt"),
        )


def _utc(expires_at: Optional[datetime]) -> Optional[datetime]:
    if expires_at is None:
        return None
    if not isinstance(expires_at, datetime):
        raise TypeError("expires_at is %s, want a datetime or NO_EXPIRY" % type(expires_at).__name__)
    if expires_at.tzinfo is None or expires_at.utcoffset() is None:
        raise ValueError("expires_at %s has no time zone; use an aware datetime such as datetime.now(timezone.utc)" % expires_at)
    return expires_at.astimezone(timezone.utc)


def _pem(data: Any) -> bytes:
    if isinstance(data, str):
        return data.encode("utf-8")
    return bytes(data or b"")


def token(token: str, expires_at: Optional[datetime]) -> Credential:
    """Returns a bearer token credential. expires_at must be an aware
    datetime; pass NO_EXPIRY for long-lived tokens."""
    return Credential(type=CREDENTIAL_TOKEN, token=token, expires_at=_utc(expires_at))


def client_cert(cert_pem: Any, key_pem: Any, expires_at: Optional[datetime]) -> Credential:
    """Returns a client certificate credential from PEM, given as bytes or
    str. expires_at must be an aware datetime, or NO_EXPIRY."""
    return Credential(type=CREDENTIAL_CLIENT_CERT, cert_data=_pem(cert_pem), key_data=_pem(key_pem), expires_at=_utc(expires_at))


def oidc(issuer: str, client_id: str, *scopes: str, client_secret: str = "") -> Credential:
    """Returns a credential that tells tug to sign in to an OpenID Connect
    issuer and use its ID token. tug always asks for the openid scope. Set
    client_secret only for issuers that require one for public clients."""
    return Credential(type=CREDENTIAL_OIDC, issuer=issuer, client_id=client_id, client_secret=client_secret, scopes=list(scopes))


def delegate(
    command: str,
    *args: str,
    env: Optional[dict[str, str]] = None,
    api_version: str = "",
    interactive_mode: str = "",
    install_hint: str = "",
    provide_cluster_info: bool = False,
    cluster_config: Any = None,
) -> Credential:
    """Returns a credential that runs another exec plugin unchanged.
    install_hint tells users how to install the command when it is
    missing. provide_cluster_info and cluster_config tell it which cluster it
    authenticates to, as ExecConfig describes."""
    return Credential(
        type=CREDENTIAL_DELEGATE,
        exec=ExecConfig(
            command=command,
            args=list(args),
            env=dict(env or {}),
            api_version=api_version,
            interactive_mode=interactive_mode,
            install_hint=install_hint,
            provide_cluster_info=provide_cluster_info,
            cluster_config=cluster_config,
        ),
    )


@dataclass
class Cluster:
    """One Kubernetes cluster a provider can reach. id is stable for the
    life of the cluster within its account. ca_data is the cluster CA in
    PEM. credential may be set when the provider already holds the
    credential at list time; tug moves it to the keychain and never writes
    it to the catalog."""

    id: str
    name: str
    region: str = ""
    version: str = ""
    endpoints: Endpoints = field(default_factory=Endpoints)
    ca_data: bytes = b""
    tls_server_name: str = ""
    insecure_skip_tls_verify: bool = False
    proxy_url: str = ""
    default_namespace: str = ""
    labels: dict[str, str] = field(default_factory=dict)
    credential: Optional[Credential] = None

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the cluster."""
        d: dict[str, Any] = {"id": self.id, "name": self.name}
        if self.region:
            d["region"] = self.region
        if self.version:
            d["version"] = self.version
        d["endpoints"] = self.endpoints.to_dict()
        if self.ca_data:
            d["caData"] = encode_bytes(self.ca_data)
        if self.tls_server_name:
            d["tlsServerName"] = self.tls_server_name
        if self.insecure_skip_tls_verify:
            d["insecureSkipTLSVerify"] = True
        if self.proxy_url:
            d["proxyURL"] = self.proxy_url
        if self.default_namespace:
            d["defaultNamespace"] = self.default_namespace
        if self.labels:
            d["labels"] = sorted_map(self.labels)
        if self.credential is not None:
            d["credential"] = self.credential.to_dict()
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "cluster") -> Cluster:
        """Reads a cluster from its JSON object."""
        f = Fields(value, where)
        cred = f.raw("credential")
        return cls(
            id=f.string("id"),
            name=f.string("name"),
            region=f.string("region"),
            version=f.string("version"),
            endpoints=Endpoints.from_dict(f.raw("endpoints"), f.path("endpoints")),
            ca_data=f.data("caData"),
            tls_server_name=f.string("tlsServerName"),
            insecure_skip_tls_verify=f.boolean("insecureSkipTLSVerify"),
            proxy_url=f.string("proxyURL"),
            default_namespace=f.string("defaultNamespace"),
            labels=f.string_map("labels"),
            credential=None if cred is None else Credential.from_dict(cred, f.path("credential")),
        )


def _empty_account() -> Account:
    return Account(id="")


def _empty_cluster() -> Cluster:
    return Cluster(id="", name="")


@dataclass
class ListRequest:
    """Asks for the clusters in one account. secrets holds the secrets the
    provider declared, by name."""

    account: Account = field(default_factory=_empty_account)
    secrets: dict[str, str] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the request, without its envelope."""
        d: dict[str, Any] = {"account": self.account.to_dict()}
        if self.secrets:
            d["secrets"] = sorted_map(self.secrets)
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "request") -> ListRequest:
        """Reads a request from its JSON object."""
        f = Fields(value, where)
        return cls(account=Account.from_dict(f.raw("account"), f.path("account")), secrets=f.string_map("secrets"))


@dataclass
class CredentialRequest:
    """Asks for a credential for one cluster. cluster_id is the id the
    provider returned from list."""

    account: Account = field(default_factory=_empty_account)
    cluster_id: str = ""
    secrets: dict[str, str] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the request, without its envelope."""
        d: dict[str, Any] = {"account": self.account.to_dict(), "clusterID": self.cluster_id}
        if self.secrets:
            d["secrets"] = sorted_map(self.secrets)
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "request") -> CredentialRequest:
        """Reads a request from its JSON object."""
        f = Fields(value, where)
        return cls(
            account=Account.from_dict(f.raw("account"), f.path("account")),
            cluster_id=f.string("clusterID"),
            secrets=f.string_map("secrets"),
        )


@dataclass
class AccessRequest:
    """Asks whether the caller can reach a cluster's API server. egress_ip
    is the caller's public address, or "" when tug does not know it."""

    account: Account = field(default_factory=_empty_account)
    cluster: Cluster = field(default_factory=_empty_cluster)
    egress_ip: str = ""
    secrets: dict[str, str] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the request, without its envelope."""
        d: dict[str, Any] = {"account": self.account.to_dict(), "cluster": self.cluster.to_dict()}
        if self.egress_ip:
            d["egressIP"] = self.egress_ip
        if self.secrets:
            d["secrets"] = sorted_map(self.secrets)
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "request") -> AccessRequest:
        """Reads a request from its JSON object."""
        f = Fields(value, where)
        return cls(
            account=Account.from_dict(f.raw("account"), f.path("account")),
            cluster=Cluster.from_dict(f.raw("cluster"), f.path("cluster")),
            egress_ip=f.string("egressIP"),
            secrets=f.string_map("secrets"),
        )


@dataclass
class Finding:
    """One result of an access check. severity is SEVERITY_INFO,
    SEVERITY_WARNING or SEVERITY_ERROR; fix says what the user can do."""

    severity: str
    message: str
    fix: str = ""

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the finding."""
        d: dict[str, Any] = {"severity": self.severity, "message": self.message}
        if self.fix:
            d["fix"] = self.fix
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "finding") -> Finding:
        """Reads a finding from its JSON object."""
        f = Fields(value, where)
        return cls(severity=f.string("severity"), message=f.string("message"), fix=f.string("fix"))
