#!/usr/bin/env python3
"""Lightweight local UTF-8 & Image X11 clipboard bridge for edualc (:78 Desktop & :79 Browser)."""

import os
import subprocess
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

ALLOWED_DISPLAYS = {"78": ":78", "79": ":79"}
MAX_PAYLOAD_BYTES = 25 * 1024 * 1024  # 25 MB for retina PNG screenshots
_holders = {}


def set_clipboard(display_key: str, data: bytes, mime_type: str = "text/plain") -> None:
    disp = ALLOWED_DISPLAYS[display_key]
    env = os.environ.copy()
    env["DISPLAY"] = disp
    # Terminate previous xclip holder we spawned for this display to avoid buildup.
    prev = _holders.get(display_key)
    if prev is not None and prev.poll() is None:
        try:
            prev.terminate()
            prev.wait(timeout=0.2)
        except Exception:
            pass

    cmd = ["xclip", "-selection", "clipboard", "-i"]
    if mime_type.startswith("image/"):
        cmd = ["xclip", "-selection", "clipboard", "-t", mime_type, "-i"]
    else:
        cmd = ["xclip", "-selection", "clipboard", "-t", "UTF8_STRING", "-i"]

    proc = subprocess.Popen(
        cmd,
        stdin=subprocess.PIPE,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        env=env,
        close_fds=True,
    )
    assert proc.stdin is not None
    proc.stdin.write(data)
    proc.stdin.close()
    _holders[display_key] = proc


def get_clipboard_targets(display_key: str) -> set[str]:
    disp = ALLOWED_DISPLAYS[display_key]
    env = os.environ.copy()
    env["DISPLAY"] = disp
    res = subprocess.run(
        ["xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"],
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        env=env,
        timeout=2,
    )
    if res.returncode != 0:
        return set()
    return {line.strip() for line in res.stdout.decode("utf-8", errors="ignore").splitlines() if line.strip()}


def get_clipboard(display_key: str, want_type: str = "text") -> tuple[bytes, str]:
    disp = ALLOWED_DISPLAYS[display_key]
    env = os.environ.copy()
    env["DISPLAY"] = disp

    if want_type in ("image", "auto"):
        targets = get_clipboard_targets(display_key)
        if "image/png" in targets and (want_type == "image" or ("UTF8_STRING" not in targets and "text/plain" not in targets)):
            res = subprocess.run(
                ["xclip", "-selection", "clipboard", "-t", "image/png", "-o"],
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                env=env,
                timeout=4,
            )
            if res.returncode == 0 and res.stdout:
                return res.stdout, "image/png"
        if want_type == "image":
            return b"", "image/png"

    res = subprocess.run(
        ["xclip", "-selection", "clipboard", "-o"],
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        env=env,
        timeout=2,
    )
    if res.returncode != 0:
        return b"", "text/plain; charset=utf-8"
    return res.stdout, "text/plain; charset=utf-8"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        return

    def _query(self) -> dict[str, list[str]]:
        return parse_qs(urlparse(self.path).query)

    def _display_key(self) -> str:
        d = (self._query().get("display") or ["78"])[0]
        return d if d in ALLOWED_DISPLAYS else "78"

    def _send_cors(self) -> None:
        origin = self.headers.get("Origin")
        if origin and (origin.startswith("http://127.0.0.1:") or origin.startswith("http://localhost:")):
            self.send_header("Access-Control-Allow-Origin", origin)
            self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
            self.send_header("Access-Control-Allow-Headers", "Content-Type")

    def do_OPTIONS(self):
        self.send_response(204)
        self._send_cors()
        self.end_headers()

    def do_GET(self):
        try:
            want_type = (self._query().get("type") or ["text"])[0].lower()
            data, content_type = get_clipboard(self._display_key(), want_type=want_type)
            self.send_response(200)
            self._send_cors()
            self.send_header("Content-Type", content_type)
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except Exception as e:
            msg = str(e).encode("utf-8")
            self.send_response(500)
            self._send_cors()
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)

    def do_POST(self):
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length > MAX_PAYLOAD_BYTES:
                self.send_response(413)
                self._send_cors()
                self.end_headers()
                return
            data = self.rfile.read(length) if length > 0 else b""
            raw_ct = (self.headers.get("Content-Type") or "text/plain").split(";", 1)[0].strip().lower()
            mime_type = raw_ct if raw_ct.startswith("image/") else "text/plain"
            set_clipboard(self._display_key(), data, mime_type=mime_type)
            self.send_response(200)
            self._send_cors()
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"ok")
        except Exception as e:
            msg = str(e).encode("utf-8")
            self.send_response(500)
            self._send_cors()
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("0.0.0.0", 6082), Handler)
    srv.serve_forever()
