"""Classified provider errors, which plugins send on the wire."""

from __future__ import annotations

import copy
import http
import re
from datetime import timedelta
from typing import Any, Optional

from ._wire import Fields

# Error codes. tug shows the code and message, and the hint on a line of its
# own; the conformance suite checks that plugins choose the right one. tug
# does not yet retry or prompt on its own because of a code, so the message
# and hint should say what the user can do.

UNAUTHENTICATED = "Unauthenticated"
"""The secrets are missing, wrong or expired. Add a hint that says how to get
valid ones."""

PERMISSION_DENIED = "PermissionDenied"
"""The identity is valid but not allowed."""

NOT_FOUND = "NotFound"
"""The account or cluster does not exist."""

RATE_LIMITED = "RateLimited"
"""Try again later. Set retry_after_seconds when the API says how long to
wait."""

UNAVAILABLE = "Unavailable"
"""The provider API could not be reached."""

INVALID_CONFIG = "InvalidConfig"
"""The account settings or the request are wrong."""

UNIMPLEMENTED = "Unimplemented"
"""The operation is not supported."""

INTERNAL = "Internal"
"""Anything else."""

# Retry-After is read as Go's strconv.Atoi reads it: digits with an optional
# sign, within a 64-bit int.
_ATOI_RE = re.compile(r"[+-]?[0-9]+\Z")
_MAX_INT64 = (1 << 63) - 1


class ProviderError(Exception):
    """A classified provider error: what a plugin sends to tug on failure.

    Raise it from a provider method. str() gives the code and message; the
    hint is for people, so tug prints it on a line of its own. Never put a
    secret in the message or the hint: tug prints and logs them.
    """

    def __init__(self, code: str, message: str, hint: str = "", retry_after_seconds: int = 0) -> None:
        super().__init__(code, message)
        self.code = code
        self.message = message
        self.hint = hint
        self.retry_after_seconds = retry_after_seconds

    def __str__(self) -> str:
        return "%s: %s" % (self.code, self.message)

    def __repr__(self) -> str:
        extra = ""
        if self.hint:
            extra += ", hint=%r" % self.hint
        if self.retry_after_seconds:
            extra += ", retry_after_seconds=%r" % self.retry_after_seconds
        return "%s(%r, %r%s)" % (type(self).__name__, self.code, self.message, extra)

    @property
    def retry_after(self) -> timedelta:
        """How long a RateLimited error suggests waiting, or zero when it does
        not say."""
        return timedelta(seconds=self.retry_after_seconds)

    def with_hint(self, hint: str) -> ProviderError:
        """Returns a copy of the error with a hint for the user."""
        c = copy.copy(self)
        c.hint = hint
        return c

    def to_dict(self) -> dict[str, Any]:
        """Returns the JSON object for the error, without its envelope. A
        missing code is sent as Internal."""
        d: dict[str, Any] = {"code": self.code or INTERNAL, "message": self.message}
        if self.hint:
            d["hint"] = self.hint
        if self.retry_after_seconds:
            d["retryAfterSeconds"] = self.retry_after_seconds
        return d

    @classmethod
    def from_dict(cls, value: Any, where: str = "error") -> ProviderError:
        """Reads an error from its JSON object. A missing code is Internal."""
        f = Fields(value, where)
        return cls(
            code=f.string("code") or INTERNAL,
            message=f.string("message"),
            hint=f.string("hint"),
            retry_after_seconds=f.integer("retryAfterSeconds"),
        )


def find_provider_error(err: Optional[BaseException]) -> Optional[ProviderError]:
    """Returns the first ProviderError in err's chain of explicit causes
    (raise ... from ...), the counterpart of Go's errors.As."""
    seen = set()
    while err is not None and id(err) not in seen:
        if isinstance(err, ProviderError):
            return err
        seen.add(id(err))
        err = err.__cause__
    return None


def code_of(err: BaseException) -> str:
    """Returns the code of err, or of the ProviderError it was raised from,
    and Internal for anything else."""
    found = find_provider_error(err)
    return found.code if found is not None else INTERNAL


def as_error(err: BaseException) -> ProviderError:
    """Converts any exception to a ProviderError, keeping the code when err
    is one or was raised from one. Anything else is Internal, with the
    exception's type and message."""
    found = find_provider_error(err)
    if found is not None:
        return found
    text = str(err)
    message = "%s: %s" % (type(err).__name__, text) if text else type(err).__name__
    return ProviderError(INTERNAL, message)


def _header(headers: Any, name: str) -> Optional[str]:
    """Returns the first value of a header from an http.client.HTTPMessage,
    an email.message.Message, a requests-style mapping or a plain dict,
    ignoring case."""
    if headers is None:
        return None
    value = headers.get(name) if hasattr(headers, "get") else None
    if value is None and hasattr(headers, "items"):
        lower = name.lower()
        for k, v in headers.items():
            if isinstance(k, str) and k.lower() == lower:
                value = v
                break
    if isinstance(value, (list, tuple)):
        value = value[0] if value else None
    if isinstance(value, bytes):
        value = value.decode("latin-1")
    return value if isinstance(value, str) else None


def _status_text(status: int) -> str:
    try:
        return http.HTTPStatus(status).phrase
    except ValueError:
        return ""


def status_error(status: int, message: str = "", headers: Any = None) -> ProviderError:
    """Converts an HTTP error status from a provider's API into a
    ProviderError with the matching code.

    401 is Unauthenticated, 403 PermissionDenied, 404 NotFound, 429
    RateLimited (with the Retry-After seconds when the API sends them), 408
    and 5xx Unavailable, 400 and 422 InvalidConfig, and anything else
    Internal. Some APIs answer 403 for a token they do not accept; plugins
    for those map it to Unauthenticated themselves. headers may be an
    http.client.HTTPMessage, as urllib returns, or any mapping.
    """
    if not message:
        message = _status_text(status)
    code, retry_after = INTERNAL, 0
    if status == 401:
        code = UNAUTHENTICATED
    elif status == 403:
        code = PERMISSION_DENIED
    elif status == 404:
        code = NOT_FOUND
    elif status == 429:
        code = RATE_LIMITED
        retry = _header(headers, "Retry-After")
        if retry is not None and _ATOI_RE.match(retry) and 0 < int(retry) <= _MAX_INT64:
            retry_after = int(retry)
    elif status == 408 or status >= 500:
        code = UNAVAILABLE
    elif status in (400, 422):
        code = INVALID_CONFIG
    return ProviderError(code, message.strip(), retry_after_seconds=retry_after)
