"""JSON helpers that read and write the protocol as Go's encoding/json does.

tug and the Go SDK define the wire format, so this module copies the parts
of Go's behaviour that show on the wire: compact output with map keys sorted,
the characters Go escapes in strings, base64 for byte fields, RFC 3339 times,
and missing or null fields read as their zero value.
"""

from __future__ import annotations

import base64
import binascii
import json
import re
from datetime import datetime, timedelta, timezone
from typing import Any, Optional

# Go escapes these inside strings so that JSON can be embedded in HTML and
# JavaScript. They can only appear inside strings, so replacing them in the
# encoded text is safe.
_GO_ESCAPES = {
    "<": "\\u003c",
    ">": "\\u003e",
    "&": "\\u0026",
    " ": "\\u2028",
    " ": "\\u2029",
}
_GO_ESCAPE_RE = re.compile("[<>&  ]")

_RFC3339_RE = re.compile(
    r"(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})\Z",
    re.ASCII,
)


class DecodeError(ValueError):
    """A JSON value does not have the shape a protocol type needs."""


def dumps(value: Any) -> bytes:
    """Encodes value as compact UTF-8 JSON, escaping strings as Go does.

    Lone surrogates, which a str can hold but UTF-8 cannot, are written as
    \\uXXXX escapes; Go reads them as U+FFFD.
    """
    text = json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    text = _GO_ESCAPE_RE.sub(lambda m: _GO_ESCAPES[m.group()], text)
    return text.encode("utf-8", "backslashreplace")


def _reject_constant(name: str) -> Any:
    raise ValueError("%s is not valid JSON" % name)


def loads(data: bytes) -> Any:
    """Decodes one JSON value. Invalid UTF-8 becomes U+FFFD, as in Go, and
    NaN and Infinity, which Python accepts but JSON does not, are refused."""
    return json.loads(data.decode("utf-8", "replace"), parse_constant=_reject_constant)


def quote(s: str) -> str:
    """Quotes s for a message, as Go's %q does for ordinary strings."""
    return json.dumps(s, ensure_ascii=False)


def sorted_map(value: Any) -> Any:
    """Returns value with the keys of every object in it sorted, which is the
    order Go writes maps in."""
    if isinstance(value, dict):
        return {k: sorted_map(value[k]) for k in sorted(value)}
    if isinstance(value, (list, tuple)):
        return [sorted_map(v) for v in value]
    return value


def encode_bytes(data: Any) -> str:
    """Encodes a byte field as standard base64. A str is taken as UTF-8 text,
    which suits PEM."""
    if isinstance(data, str):
        data = data.encode("utf-8")
    return base64.b64encode(bytes(data)).decode("ascii")


def format_time(t: datetime) -> str:
    """Formats an aware datetime as RFC 3339, as Go formats a time.Time:
    "Z" for UTC and no trailing zeros in the fraction."""
    offset = t.utcoffset() if t.tzinfo is not None else None
    if offset is None:
        raise ValueError("time %s has no time zone; use an aware datetime such as datetime.now(timezone.utc)" % t)
    s = "%04d-%02d-%02dT%02d:%02d:%02d" % (t.year, t.month, t.day, t.hour, t.minute, t.second)
    if t.microsecond:
        s += "." + ("%06d" % t.microsecond).rstrip("0")
    minutes = int(offset.total_seconds()) // 60
    if minutes == 0:
        return s + "Z"
    sign = "-" if minutes < 0 else "+"
    return s + "%s%02d:%02d" % (sign, abs(minutes) // 60, abs(minutes) % 60)


def parse_time(s: str) -> datetime:
    """Parses an RFC 3339 time. Fractions finer than a microsecond, which
    datetime cannot hold, are truncated."""
    m = _RFC3339_RE.match(s)
    if not m:
        raise ValueError("%s is not an RFC 3339 time" % quote(s))
    year, month, day, hour, minute, second = (int(g) for g in m.groups()[:6])
    fraction, zone = m.group(7), m.group(8)
    micro = int((fraction or "0")[:6].ljust(6, "0"))
    if zone == "Z":
        tz = timezone.utc
    else:
        hours, minutes = int(zone[1:3]), int(zone[4:6])
        delta = timedelta(hours=hours, minutes=minutes)
        tz = timezone(-delta if zone[0] == "-" else delta)
    try:
        return datetime(year, month, day, hour, minute, second, micro, tzinfo=tz)
    except ValueError as e:
        raise ValueError("%s is not an RFC 3339 time: %s" % (quote(s), e)) from None


def json_type(value: Any) -> str:
    """Names the JSON type of a decoded value, for messages."""
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "a boolean"
    if isinstance(value, (int, float)):
        return "a number"
    if isinstance(value, str):
        return "a string"
    if isinstance(value, list):
        return "an array"
    if isinstance(value, dict):
        return "an object"
    return type(value).__name__


class Fields:
    """Reads the fields of one decoded JSON object as Go decodes a struct:
    a missing or null field has its zero value, unknown fields are ignored,
    and a field of the wrong type is a DecodeError."""

    def __init__(self, value: Any, where: str = "") -> None:
        if value is None:
            value = {}
        if not isinstance(value, dict):
            raise DecodeError("%s is %s, want an object" % (where or "the message", json_type(value)))
        self._obj = value
        self._where = where

    def path(self, key: str) -> str:
        """Returns the dotted path of key, for messages. where is "" for the
        message itself."""
        return self._where + "." + key if self._where else key

    def _wrong(self, key: str, want: str, value: Any) -> DecodeError:
        return DecodeError("%s is %s, want %s" % (self.path(key), json_type(value), want))

    def raw(self, key: str) -> Any:
        """Returns the field as decoded, or None."""
        return self._obj.get(key)

    def string(self, key: str) -> str:
        """Returns a string field, or ""."""
        v = self._obj.get(key)
        if v is None:
            return ""
        if not isinstance(v, str):
            raise self._wrong(key, "a string", v)
        return v

    def boolean(self, key: str) -> bool:
        """Returns a boolean field, or False."""
        v = self._obj.get(key)
        if v is None:
            return False
        if not isinstance(v, bool):
            raise self._wrong(key, "a boolean", v)
        return v

    def integer(self, key: str) -> int:
        """Returns an integer field, or 0. As in Go, 1.0 is not an integer."""
        v = self._obj.get(key)
        if v is None:
            return 0
        if isinstance(v, bool) or not isinstance(v, int):
            raise self._wrong(key, "an integer", v)
        return v

    def data(self, key: str) -> bytes:
        """Returns a base64 byte field, or b""."""
        v = self.string(key)
        if not v:
            return b""
        try:
            # Go's decoder skips line breaks in base64 and nothing else.
            return base64.b64decode(v.replace("\r", "").replace("\n", ""), validate=True)
        except (binascii.Error, ValueError) as e:
            raise DecodeError("%s is not base64: %s" % (self.path(key), e)) from None

    def time(self, key: str) -> Optional[datetime]:
        """Returns an RFC 3339 time field, or None."""
        v = self._obj.get(key)
        if v is None:
            return None
        if not isinstance(v, str):
            raise self._wrong(key, "an RFC 3339 time", v)
        try:
            return parse_time(v)
        except ValueError as e:
            raise DecodeError("%s: %s" % (self.path(key), e)) from None

    def string_list(self, key: str) -> list[str]:
        """Returns an array of strings, or []."""
        v = self._obj.get(key)
        if v is None:
            return []
        if not isinstance(v, list):
            raise self._wrong(key, "an array of strings", v)
        out = []
        for i, item in enumerate(v):
            if item is None:
                item = ""
            if not isinstance(item, str):
                raise DecodeError("%s[%d] is %s, want a string" % (self.path(key), i, json_type(item)))
            out.append(item)
        return out

    def string_map(self, key: str) -> dict[str, str]:
        """Returns an object of strings, or {}."""
        v = self._obj.get(key)
        if v is None:
            return {}
        if not isinstance(v, dict):
            raise self._wrong(key, "an object of strings", v)
        out = {}
        for k, item in v.items():
            if item is None:
                item = ""
            if not isinstance(item, str):
                raise DecodeError("%s.%s is %s, want a string" % (self.path(key), k, json_type(item)))
            out[k] = item
        return out

    def any_map(self, key: str) -> dict[str, Any]:
        """Returns an object of any JSON values, or {}."""
        v = self._obj.get(key)
        if v is None:
            return {}
        if not isinstance(v, dict):
            raise self._wrong(key, "an object", v)
        return v

    def array(self, key: str) -> list[Any]:
        """Returns an array of any JSON values, or []."""
        v = self._obj.get(key)
        if v is None:
            return []
        if not isinstance(v, list):
            raise self._wrong(key, "an array", v)
        return v
