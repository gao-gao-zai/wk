#!/usr/bin/env python3
"""Isolated Linux integration test. Requires host-built wb2api and gateway paths.
Uses an empty account pool and temporary state; never contacts a provider.
"""
import http.client
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
import urllib.parse


def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Unix(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=3)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(3)
        self.sock.connect(self.path)


def ctl(path, action="status", release="a", params=None):
    c = Unix(str(path))
    try:
        query = urllib.parse.urlencode(params or {"release": release})
        c.request("GET" if action == "status" else "POST", f"/{action}?{query}")
        r = c.getresponse()
        data = r.read()
        assert r.status in (200, 204), (action, r.status, data)
        return json.loads(data) if action == "status" else None
    finally:
        c.close()


def wait(predicate):
    end = time.monotonic() + 20
    while not predicate():
        assert time.monotonic() < end, "integration deadline"
        time.sleep(0.05)


def run(server, gateway):
    children = []
    with tempfile.TemporaryDirectory(prefix="wb-upgrade-") as tmp:
        root = Path(tmp)
        config = root / "config.json"
        config.write_text(json.dumps({"api_key": "integration-only-api-key", "frontend_password": "integration-only-password",
                                      "auth_dir": str(root / "auths"), "state_file": str(root / "data/state.json")}))
        (root / "auths").mkdir()
        before = sorted(str(p.relative_to(root)) for p in root.rglob("*"))
        result = subprocess.run([server, "-config", str(config), "-preflight"], capture_output=True)
        assert result.returncode == 0, "preflight failed"
        assert before == sorted(str(p.relative_to(root)) for p in root.rglob("*")), "preflight wrote state"
        lock = root / "writer.lock"
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def start(release, listen):
            path = root / (release + ".sock")
            env = dict(os.environ, WB2A_LISTEN=f"127.0.0.1:{listen}")
            p = subprocess.Popen([server, "-config", str(config), "-release", release, "-control", str(path), "-state-lock", str(lock)],
                                 env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            children.append(p)
            return p, path

        def ready(path):
            try:
                return ctl(path)["ready"]
            except (OSError, http.client.HTTPException):
                return False

        try:
            a_port = port()
            a, a_ctl = start("a", a_port)
            wait(lambda: ready(a_ctl))
            g_port, g_ctl = port(), root / "gateway.sock"
            g = subprocess.Popen([gateway, "-listen", f"127.0.0.1:{g_port}", "-target", f"http://127.0.0.1:{a_port}",
                                  "-release", "a", "-control", str(g_ctl), "-wait", "20s"],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            children.append(g)
            wait(lambda: g_ctl.exists())
            req = urllib.request.Request(f"http://127.0.0.1:{a_port}/admin/unlock", data=b'{"password":"integration-only-password"}',
                                         headers={"Content-Type": "application/json"})
            with opener.open(req) as response:
                cookie = response.headers["Set-Cookie"].split(";", 1)[0]
            b, _ = start("duplicate", port())
            assert b.wait(timeout=5) != 0, "second writer admitted"
            ctl(a_ctl, "pause")
            ctl(a_ctl, "quiesce")
            wait(lambda: ctl(a_ctl)["background"] == 0)
            ctl(a_ctl, "resume")
            assert ctl(a_ctl)["ready"], "abort did not restore readiness"
            ctl(g_ctl, "hold", params={"expected": "a"})
            executor = ThreadPoolExecutor(max_workers=1)

            def queued_request():
                request = urllib.request.Request(f"http://127.0.0.1:{g_port}/admin/config", headers={"Cookie": cookie})
                with opener.open(request, timeout=20) as response:
                    return response.status, response.headers["X-WorkBuddy-Release"]

            queued = executor.submit(queued_request)
            wait(lambda: ctl(g_ctl)["pending"] == 1)
            assert not queued.done(), "gateway did not hold new request"
            ctl(a_ctl, "pause")
            ctl(a_ctl, "quiesce")
            wait(lambda: ctl(a_ctl)["background"] == 0)
            ctl(a_ctl, "stop")
            assert a.wait(timeout=20) == 0
            assert (root / "data/upgrade-handoff.enc").exists()
            b_port = port()
            b, b_ctl = start("b", b_port)
            wait(lambda: ready(b_ctl))
            ctl(g_ctl, "switch", params={"expected": "a", "next": "b", "target": f"http://127.0.0.1:{b_port}"})
            ctl(g_ctl, "resume", params={"expected": "b"})
            assert queued.result(timeout=20) == (200, "b"), "queued request/session not handed to new release"
            executor.shutdown()
            req = urllib.request.Request(f"http://127.0.0.1:{b_port}/admin/config", headers={"Cookie": cookie})
            with opener.open(req) as response:
                assert response.status == 200, "frontend session lost"
                assert response.headers["X-WorkBuddy-Release"] == "b"
            ctl(b_ctl, "pause", "b")
            ctl(b_ctl, "quiesce", "b")
            wait(lambda: ctl(b_ctl)["background"] == 0)
            ctl(b_ctl, "stop", "b")
            assert b.wait(timeout=20) == 0
            print("PASS: read-only preflight, exclusive writer, reversible drain, clean exit, gateway queue/switch, session handoff")
        finally:
            for child in children:
                if child.poll() is None:
                    child.terminate()
                    try:
                        child.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        child.kill()
                        child.wait()


if __name__ == "__main__":
    run(sys.argv[1], sys.argv[2])
