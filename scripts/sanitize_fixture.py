#!/usr/bin/env python3
"""Sanitize a raw yt-dlp / gallery-dl capture into a committable test fixture.

Read docs/testing-contract.md before using this. The short version:

  * Nothing that came off the wire is committed unmodified.
  * Every credential-shaped value is replaced with an obviously synthetic
    placeholder, never deleted -- the *shape* has to survive so the redaction
    tests in internal/archivers have something real to redact.
  * Bulk fields yt-dlp emits but Arker never reads (formats, thumbnails,
    heatmap, captions) are truncated, not dropped, so the structure stays
    faithful while the file stays reviewable.

Usage:
    scripts/sanitize_fixture.py ytdlp  RAW.info.json  OUT.info.json
    scripts/sanitize_fixture.py gallery RAW_SIDECAR.json OUT_SIDECAR.json
"""

import json
import re
import sys

# RFC 5737 TEST-NET-3. Any IP-shaped value becomes this: it can never route,
# so a fixture that leaks one leaks nothing.
FAKE_IP = "203.0.113.7"

# A fixed, obviously-fake stand-in. Tests assert these strings are absent from
# anything Arker persists, so they must be distinctive.
def synth(kind):
    return "SYNTHETIC-%s-NOT-A-REAL-SECRET" % kind.upper()


# Query parameters that carry a signature, a session, or a client identity.
# Superset of internal/archivers/video_metadata.go:sensitiveURLParameter so the
# fixture is clean even where Arker would not have redacted.
SIGNED_PARAMS = {
    "sig", "lsig", "signature", "auth", "key", "api_key", "apikey", "pot",
    "expire", "ip", "ipbits", "hmac", "jwt", "policy", "token", "credential",
    "hdntl", "hdnea", "hdnts", "ei", "n", "c", "cpn", "id", "mh", "ms", "mv",
    "mn", "gir", "clen", "dur", "lmt", "mt", "txp", "sparams", "requiressl",
    "efont", "met", "rms", "bui", "spc", "vprv", "svpuc", "xpc", "sc",
    "_nc_ht", "_nc_cat", "_nc_ohc", "_nc_gid", "_nc_sid", "oh", "oe", "ccb",
}

# Belt and braces: any parameter whose *name* looks derived from a signature or
# an opaque CDN blob, whatever the exact spelling. YouTube alone ships sig,
# sigh, sqp, and lsig for the same job.
SIGNED_PARAM_RE = re.compile(r"(sig|sqp|hash|nonce|_nc_|hdn|sec|tok)", re.IGNORECASE)

# Any value longer than this in a query string is assumed opaque and replaced.
# Real, meaningful parameters (itag=137, mime=video%2Fmp4) are all short.
MAX_QUERY_VALUE = 24

# Keys whose *value* is replaced wholesale regardless of shape.
SENSITIVE_KEYS = re.compile(
    r"(authorization|cookie|password|passwd|proxy|credential|secret|api[_-]?key"
    r"|access[_-]?token|refresh[_-]?token|session[_-]?token|session[_-]?id"
    r"|visitor[_-]?data|visitor[_-]?id|po_?token|^token$|_token$|^token_)",
    re.IGNORECASE,
)

IP_RE = re.compile(r"\b(?:\d{1,3}\.){3}\d{1,3}\b")

# yt-dlp fields Arker never reads that dominate the byte count. Truncated to
# this many elements so the key and its element shape survive.
TRUNCATE_LISTS = {
    "formats": 2,
    "thumbnails": 2,
    "requested_formats": 2,
    "heatmap": 0,
    "chapters": 1,
    "_format_sort_fields": 3,
    "comments": 1,
}
DROP_KEYS = {"automatic_captions", "subtitles", "requested_subtitles"}

MAX_STRING = 400


def sanitize_url(value):
    """Rewrite a URL so no signature, token, or address survives."""
    if "://" not in value:
        return value
    head, _, query = value.partition("?")
    # Strip embedded credentials: scheme://user:pass@host -> scheme://host
    head = re.sub(r"://[^/@]+@", "://", head)
    head = IP_RE.sub(FAKE_IP, head)
    if not query:
        return head
    parts = []
    for pair in query.split("&"):
        key, sep, val = pair.partition("=")
        if not sep:
            parts.append(pair)
            continue
        if (key.lower() in SIGNED_PARAMS or SIGNED_PARAM_RE.search(key)
                or len(val) > MAX_QUERY_VALUE):
            val = synth(key or "param")
        parts.append("%s=%s" % (key, val))
    return head + "?" + "&".join(parts)


def pick_representative(key, items, limit):
    """Keep the most informative `limit` entries of a truncated list.

    Taking the first N off yt-dlp's `formats` yields storyboard tiles, which
    teach a reader nothing about what a real media format looks like. Prefer
    entries that carry an actual media URL, and prefer one video plus one audio
    so the "bestvideo+bestaudio" merge Arker asks for is visible in the fixture.
    """
    if limit <= 0 or not items:
        return []
    if key != "formats":
        return items[:limit]

    def is_media(entry):
        return isinstance(entry, dict) and entry.get("protocol") in (
            "https", "http", "m3u8_native", "http_dash_segments")

    video = [e for e in items if is_media(e) and e.get("vcodec", "none") != "none"]
    audio = [e for e in items if is_media(e) and e.get("acodec", "none") != "none"
             and e.get("vcodec", "none") == "none"]
    chosen = video[-1:] + audio[-1:]
    if not chosen:
        chosen = [e for e in items if is_media(e)][:limit] or items[:limit]
    return chosen[:limit]


def sanitize(value, key=None):
    if isinstance(value, dict):
        out = {}
        for k, v in value.items():
            if k in DROP_KEYS:
                continue
            if SENSITIVE_KEYS.search(k):
                out[k] = synth(k)
                continue
            if k in TRUNCATE_LISTS and isinstance(v, list):
                kept = pick_representative(k, v, TRUNCATE_LISTS[k])
                out[k] = [sanitize(item, k) for item in kept]
                continue
            out[k] = sanitize(v, k)
        return out
    if isinstance(value, list):
        return [sanitize(item, key) for item in value]
    if isinstance(value, str):
        if "://" in value:
            return sanitize_url(value)
        if IP_RE.fullmatch(value.strip()):
            return FAKE_IP
        if len(value) > MAX_STRING:
            return value[:MAX_STRING] + " [truncated for fixture]"
        return value
    return value


def main():
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    _, kind, src, dst = sys.argv
    if kind not in ("ytdlp", "gallery"):
        sys.exit("kind must be 'ytdlp' or 'gallery'")
    with open(src) as fh:
        data = json.load(fh)
    clean = sanitize(data)
    with open(dst, "w") as fh:
        json.dump(clean, fh, indent=2, sort_keys=True, ensure_ascii=False)
        fh.write("\n")
    print("wrote %s (%d bytes)" % (dst, len(json.dumps(clean))))


if __name__ == "__main__":
    main()
