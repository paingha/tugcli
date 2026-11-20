"""The Python SDK for writing tug provider plugins.

A provider is anything that can list Kubernetes clusters and hand out
credentials for them. A plugin is an executable named tug-provider-<name>.
For each operation tug starts it with the operation as its only argument,
writes one JSON request to stdin and reads one JSON response from stdout.
In serve mode one process answers many requests, one JSON line each.
Anything written to stderr goes to tug's log.

Most plugins subclass Provider and hand it to main::

    import sys
    from tug_provider import Provider, main

    class AcmeProvider(Provider):
        def info(self): ...
        def list(self, request): ...
        def credential(self, request): ...

    if __name__ == "__main__":
        sys.exit(main(AcmeProvider()))

The package mirrors the Go SDK, github.com/paingha/tugcli/pkg/provider, and
writes the same JSON. It uses the standard library only.
"""

from ._errors import (
    INTERNAL,
    INVALID_CONFIG,
    NOT_FOUND,
    PERMISSION_DENIED,
    RATE_LIMITED,
    UNAUTHENTICATED,
    UNAVAILABLE,
    UNIMPLEMENTED,
    ProviderError,
    as_error,
    code_of,
    status_error,
)
from ._protocol import (
    MAX_MESSAGE_BYTES,
    OP_ACCOUNTS,
    OP_CHECK_ACCESS,
    OP_CREDENTIAL,
    OP_DESCRIBE,
    OP_LIST,
    OP_ROTATE,
    OP_SERVE,
    decode_accounts,
    decode_check_access,
    decode_credential,
    decode_describe,
    decode_list,
    encode_request,
    is_known_op,
    supported_api_versions,
)
from ._main import main, run
from ._provider import EXIT_ERROR, EXIT_OK, EXIT_USAGE, Canceled, Provider, canceled, capabilities, sleep
from ._types import (
    API_VERSION,
    CAP_ACCOUNTS,
    CAP_CHECK_ACCESS,
    CAP_CREDENTIAL,
    CAP_LIST,
    CAP_ROTATE,
    CAP_SERVE,
    CAPABILITY_NAMES,
    CREDENTIAL_CLIENT_CERT,
    CREDENTIAL_DELEGATE,
    CREDENTIAL_OIDC,
    CREDENTIAL_TOKEN,
    NO_EXPIRY,
    SEVERITY_ERROR,
    SEVERITY_INFO,
    SEVERITY_WARNING,
    AccessRequest,
    Account,
    Cluster,
    Credential,
    CredentialRequest,
    Description,
    Endpoints,
    ExecConfig,
    Finding,
    Info,
    ListRequest,
    SecretSpec,
    client_cert,
    delegate,
    oidc,
    token,
)

__version__ = "0.1.0"

__all__ = [
    # Protocol
    "API_VERSION",
    "MAX_MESSAGE_BYTES",
    "OP_ACCOUNTS",
    "OP_CHECK_ACCESS",
    "OP_CREDENTIAL",
    "OP_DESCRIBE",
    "OP_LIST",
    "OP_ROTATE",
    "OP_SERVE",
    "supported_api_versions",
    "is_known_op",
    # Provider and main
    "Provider",
    "capabilities",
    "main",
    "run",
    "Canceled",
    "canceled",
    "sleep",
    "EXIT_OK",
    "EXIT_ERROR",
    "EXIT_USAGE",
    # Capabilities
    "CAP_ACCOUNTS",
    "CAP_LIST",
    "CAP_CREDENTIAL",
    "CAP_ROTATE",
    "CAP_CHECK_ACCESS",
    "CAP_SERVE",
    "CAPABILITY_NAMES",
    # Types
    "Info",
    "SecretSpec",
    "Description",
    "Account",
    "Endpoints",
    "Cluster",
    "ExecConfig",
    "Credential",
    "ListRequest",
    "CredentialRequest",
    "AccessRequest",
    "Finding",
    "SEVERITY_INFO",
    "SEVERITY_WARNING",
    "SEVERITY_ERROR",
    # Credentials
    "CREDENTIAL_TOKEN",
    "CREDENTIAL_CLIENT_CERT",
    "CREDENTIAL_OIDC",
    "CREDENTIAL_DELEGATE",
    "NO_EXPIRY",
    "token",
    "client_cert",
    "oidc",
    "delegate",
    # Errors
    "ProviderError",
    "UNAUTHENTICATED",
    "PERMISSION_DENIED",
    "NOT_FOUND",
    "RATE_LIMITED",
    "UNAVAILABLE",
    "INVALID_CONFIG",
    "UNIMPLEMENTED",
    "INTERNAL",
    "code_of",
    "as_error",
    "status_error",
    # Testing and hosts
    "encode_request",
    "decode_describe",
    "decode_accounts",
    "decode_list",
    "decode_credential",
    "decode_check_access",
]
