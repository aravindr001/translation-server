#!/usr/bin/env python3
"""Stub worker used exclusively by the Go tests.

It speaks just enough HTTP over the Unix socket to exercise the gateway state
machine. All behavior is controlled by environment variables.
"""

from __future__ import annotations

import json
import os
import signal
import socketserver
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler

SOCKET_PATH = os.environ["TRANSLATOR_SOCKET"]
SPAWN_LOG = os.environ.get("FAKE_WORKER_SPAWN_LOG", "")
READY_DELAY = float(os.environ.get("FAKE_READY_DELAY", "0"))
FAIL_START = os.environ.get("FAKE_FAIL_START", "0") == "1"
EXIT_AFTER_READY = float(os.environ.get("FAKE_EXIT_AFTER_READY", "0"))
CRASH_AFTER_READY = os.environ.get("FAKE_CRASH_AFTER_READY", "0") == "1"
TRANSLATE_DELAY = float(os.environ.get("FAKE_TRANSLATE_DELAY", "0"))
HANG_TRANSLATE = os.environ.get("FAKE_HANG_TRANSLATE", "0") == "1"
CRASH_ON_TRANSLATE = os.environ.get("FAKE_CRASH_ON_TRANSLATE", "0") == "1"
TERM_DELAY = float(os.environ.get("FAKE_TERM_DELAY", "0"))


def append_spawn() -> None:
    if not SPAWN_LOG:
        return
    fd = os.open(SPAWN_LOG, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        os.write(fd, f"{os.getpid()}\n".encode("ascii"))
    finally:
        os.close(fd)


append_spawn()
if FAIL_START:
    sys.exit(3)

os.umask(0o077)
try:
    os.unlink(SOCKET_PATH)
except FileNotFoundError:
    pass

state = {"ready": False}
state_lock = threading.Lock()


def mark_ready_after_delay() -> None:
    if READY_DELAY:
        time.sleep(READY_DELAY)
    with state_lock:
        state["ready"] = True
    if EXIT_AFTER_READY:
        time.sleep(EXIT_AFTER_READY)
        os._exit(7)
    if CRASH_AFTER_READY:
        time.sleep(0.1)
        os._exit(8)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format: str, *args) -> None:  # keep test output quiet
        return

    def send_json(self, code: int, payload: dict) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            self.wfile.write(body)
        except BrokenPipeError:
            pass

    def do_GET(self) -> None:
        if self.path != "/ready":
            self.send_json(404, {"error": "not found"})
            return
        with state_lock:
            ready = state["ready"]
        if ready:
            self.send_json(200, {"ready": True})
        else:
            self.send_json(503, {"ready": False})

    def do_POST(self) -> None:
        if self.path != "/translate":
            self.send_json(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        try:
            payload = json.loads(raw.decode("utf-8"))
            text = payload["text"]
        except (UnicodeDecodeError, json.JSONDecodeError, KeyError, TypeError):
            self.send_json(400, {"error": "invalid request"})
            return
        with state_lock:
            ready = state["ready"]
        if not ready:
            self.send_json(503, {"error": "not ready"})
            return
        if CRASH_ON_TRANSLATE:
            os.kill(os.getpid(), signal.SIGKILL)
            return
        if HANG_TRANSLATE:
            time.sleep(3600)
        if TRANSLATE_DELAY:
            time.sleep(TRANSLATE_DELAY)
        self.send_json(200, {"translation": "ML:" + text})


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


def on_term(_signum, _frame) -> None:
    if TERM_DELAY:
        time.sleep(TERM_DELAY)
    try:
        os.unlink(SOCKET_PATH)
    except OSError:
        pass
    os._exit(0)


signal.signal(signal.SIGTERM, on_term)
signal.signal(signal.SIGINT, on_term)

threading.Thread(target=mark_ready_after_delay, daemon=True).start()
with Server(SOCKET_PATH, Handler) as server:
    server.serve_forever(poll_interval=0.05)
