#!/usr/bin/env python3
"""Lightweight local UTF-8 X11 clipboard bridge for edualc (:78 Desktop & :79 Browser)."""

import os
import subprocess
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

ALLOWED_DISPLAYS = {"78": ":78", "79": ":79"}
_holders = {}


def set_clipboard(display_key: str, data: bytes) -> None:
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
    proc = subprocess.Popen(
        ["xclip", "-selection", "clipboard", "-i"],
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


def get_clipboard(display_key: str) -> bytes:
    disp = ALLOWED_DISPLAYS[display_key]
    env = os.environ.copy()
    env["DISPLAY"] = disp
    res = subprocess.run(
        ["xclip", "-selection", "clipboard", "-o"],
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        env=env,
        timeout=2,
    )
    if res.returncode != 0:
        return b""
    return res.stdout


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        return

    def _display_key(self) -> str:
        qs = parse_qs(urlparse(self.path).query)
        d = (qs.get("display") or ["78"])[0]
        return d if d in ALLOWED_DISPLAYS else "78"

    def do_GET(self):
        try:
            data = get_clipboard(self._display_key())
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except Exception as e:
            msg = str(e).encode("utf-8")
            self.send_response(500)
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)

    def do_POST(self):
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length > 2 * 1024 * 1024:
                self.send_response(413)
                self.end_headers()
                return
            data = self.rfile.read(length) if length > 0 else b""
            set_clipboard(self._display_key(), data)
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"ok")
        except Exception as e:
            msg = str(e).encode("utf-8")
            self.send_response(500)
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("0.0.0.0", 6082), Handler)
    srv.serve_forever()
