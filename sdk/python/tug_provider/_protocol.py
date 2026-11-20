"""Operations, message envelopes, and the encoding a host uses.

encode_request builds the request tug writes to a plugin's stdin, and the
decode functions read a response the way tug does, so plugin tests can call
an operation exactly as tug would.
"""

from __future__ import annotations

from typing import Any, Union

from ._errors import INTERNAL, ProviderError
from ._types import (
    API_VERSION,
    AccessRequest,
    Account,
    Cluster,
    Credential,
    CredentialRequest,
    Description,
    Finding,
    ListRequest,
)
from ._wire import DecodeError, Fields, dumps, json_type, loads, quote

# Operations, passed to the plugin as its only argument.
OP_DESCRIBE = "describe"
OP_ACCOUNTS = "accounts"
OP_LIST = "list"
OP_CREDENTIAL = "credential"
OP_ROTATE = "rotate"
OP_CHECK_ACCESS = "checkAccess"
OP_SERVE = "serve"

MAX_MESSAGE_BYTES = 16 << 20
"""The largest request or response, in bytes."""

# Message kinds.
KIND_DESCRIBE_REQUEST = "DescribeRequest"
KIND_DESCRIBE_RESPONSE = "DescribeResponse"
KIND_ACCOUNTS_REQUEST = "AccountsRequest"
KIND_ACCOUNTS_RESPONSE = "AccountsResponse"
KIND_LIST_REQUEST = "ListRequest"
KIND_LIST_RESPONSE = "ListResponse"
KIND_CREDENTIAL_REQUEST = "CredentialRequest"
KIND_ROTATE_REQUEST = "RotateRequest"
KIND_CREDENTIAL_RESPONSE = "CredentialResponse"
KIND_CHECK_ACCESS_REQUEST = "CheckAccessRequest"
KIND_CHECK_ACCESS_RESPONSE = "CheckAccessResponse"
KIND_ERROR = "Error"

_OPS = (OP_DESCRIBE, OP_ACCOUNTS, OP_LIST, OP_CREDENTIAL, OP_ROTATE, OP_CHECK_ACCESS, OP_SERVE)


def supported_api_versions() -> list[str]:
    """Lists every protocol version this SDK can answer, newest first. tug
    picks the highest version both sides support."""
    return [API_VERSION]


def is_known_op(op: str) -> bool:
    """Reports whether op is a protocol operation."""
    return op in _OPS


def envelope(kind: str, payload: dict[str, Any]) -> dict[str, Any]:
    """Returns payload with apiVersion and kind in front, as every message
    carries them."""
    msg: dict[str, Any] = {"apiVersion": API_VERSION, "kind": kind}
    msg.update(payload)
    return msg


def encode_request(op: str, payload: Any = None) -> bytes:
    """Returns the request message a host writes to a plugin's stdin for op.

    payload is ignored for describe and accounts, and must be a ListRequest
    for list, a CredentialRequest for credential and rotate, and an
    AccessRequest for checkAccess.
    """
    if op == OP_DESCRIBE:
        msg = envelope(KIND_DESCRIBE_REQUEST, {})
    elif op == OP_ACCOUNTS:
        msg = envelope(KIND_ACCOUNTS_REQUEST, {})
    elif op == OP_LIST:
        if not isinstance(payload, ListRequest):
            raise TypeError("list needs a ListRequest, got %s" % type(payload).__name__)
        msg = envelope(KIND_LIST_REQUEST, payload.to_dict())
    elif op in (OP_CREDENTIAL, OP_ROTATE):
        if not isinstance(payload, CredentialRequest):
            raise TypeError("%s needs a CredentialRequest, got %s" % (op, type(payload).__name__))
        kind = KIND_ROTATE_REQUEST if op == OP_ROTATE else KIND_CREDENTIAL_REQUEST
        msg = envelope(kind, payload.to_dict())
    elif op == OP_CHECK_ACCESS:
        if not isinstance(payload, AccessRequest):
            raise TypeError("checkAccess needs an AccessRequest, got %s" % type(payload).__name__)
        msg = envelope(KIND_CHECK_ACCESS_REQUEST, payload.to_dict())
    else:
        raise ValueError("unknown operation %s" % quote(op))
    return dumps(msg)


Response = Union[bytes, bytearray, str]


def _decode_response(data: Response, want_kind: str) -> dict[str, Any]:
    """Checks a response's envelope and returns the decoded message. An Error
    message is raised as a ProviderError, and anything malformed is
    Internal."""
    if isinstance(data, str):
        data = data.encode("utf-8")
    try:
        msg = loads(bytes(data))
    except ValueError as e:
        raise ProviderError(INTERNAL, "response is not JSON: %s" % e) from None
    if not isinstance(msg, dict):
        raise ProviderError(INTERNAL, "response is %s, want an object" % json_type(msg))
    kind, version = msg.get("kind"), msg.get("apiVersion")
    if kind == KIND_ERROR:
        try:
            err = ProviderError.from_dict(msg, "")
        except DecodeError as e:
            raise ProviderError(INTERNAL, "malformed error response: %s" % e) from None
        raise err
    if version not in supported_api_versions():
        raise ProviderError(INTERNAL, "unsupported apiVersion %s" % quote(str(version or "")))
    if kind != want_kind:
        raise ProviderError(INTERNAL, "expected kind %s, got %s" % (want_kind, quote(str(kind or ""))))
    return msg


def _malformed(kind: str, e: ValueError) -> ProviderError:
    return ProviderError(INTERNAL, "malformed %s: %s" % (kind, e))


def decode_describe(data: Response) -> Description:
    """Decodes a describe response."""
    msg = _decode_response(data, KIND_DESCRIBE_RESPONSE)
    try:
        return Description.from_dict(msg, "")
    except DecodeError as e:
        raise _malformed(KIND_DESCRIBE_RESPONSE, e) from None


def decode_accounts(data: Response) -> list[Account]:
    """Decodes an accounts response."""
    f = Fields(_decode_response(data, KIND_ACCOUNTS_RESPONSE))
    try:
        return [Account.from_dict(a, "accounts[%d]" % i) for i, a in enumerate(f.array("accounts"))]
    except DecodeError as e:
        raise _malformed(KIND_ACCOUNTS_RESPONSE, e) from None


def decode_list(data: Response) -> list[Cluster]:
    """Decodes a list response."""
    f = Fields(_decode_response(data, KIND_LIST_RESPONSE))
    try:
        return [Cluster.from_dict(c, "clusters[%d]" % i) for i, c in enumerate(f.array("clusters"))]
    except DecodeError as e:
        raise _malformed(KIND_LIST_RESPONSE, e) from None


def decode_credential(data: Response) -> Credential:
    """Decodes a credential or rotate response and validates the
    credential."""
    f = Fields(_decode_response(data, KIND_CREDENTIAL_RESPONSE))
    try:
        cred = Credential.from_dict(f.raw("credential"), "credential")
    except DecodeError as e:
        raise _malformed(KIND_CREDENTIAL_RESPONSE, e) from None
    try:
        cred.validate()
    except ValueError as e:
        raise ProviderError(INTERNAL, "plugin returned an invalid credential: %s" % e) from None
    return cred


def decode_check_access(data: Response) -> list[Finding]:
    """Decodes a checkAccess response."""
    f = Fields(_decode_response(data, KIND_CHECK_ACCESS_RESPONSE))
    try:
        return [Finding.from_dict(x, "findings[%d]" % i) for i, x in enumerate(f.array("findings"))]
    except DecodeError as e:
        raise _malformed(KIND_CHECK_ACCESS_RESPONSE, e) from None
