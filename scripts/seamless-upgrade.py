#!/usr/bin/env python3
"""Single-host, single-writer release handoff. No credentials on the command line.

Containers must be prepared with immutable images, identical persistent mounts,
unique release/control paths and restart policy 'no' for the retired release.
Only Linux host execution is supported. See README's seamless upgrade section.
"""
import argparse
import fcntl
import http.client
import json
import os
import socket
import subprocess
import time
import urllib.parse
import urllib.request


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=10)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def call(path, action="status", **params):
    conn = UnixHTTP(path)
    try:
        route = "/" + action
        if params:
            route += "?" + urllib.parse.urlencode(params)
        conn.request("GET" if action == "status" else "POST", route)
        response = conn.getresponse()
        body = response.read(65536)
        if response.status >= 300:
            raise RuntimeError(f"control {action}: HTTP {response.status}")
        return json.loads(body) if action == "status" else None
    finally:
        conn.close()


def docker(*args):
    result = subprocess.run(["docker", *args], capture_output=True, text=True)
    if result.returncode:
        # Docker errors may contain environment/configuration details.
        raise RuntimeError(f"docker {args[0]} failed (exit {result.returncode})")
    return result.stdout.strip()


def inspect(name):
    return json.loads(docker("inspect", name))[0]


def argument(info, flag):
    args = info["Config"].get("Cmd") or []
    for i, value in enumerate(args):
        if value == flag and i + 1 < len(args):
            return args[i + 1]
        if value.startswith(flag + "="):
            return value.split("=", 1)[1]
    raise RuntimeError(f"missing container argument {flag}")


def validate_containers(args):
    old, new, check = map(inspect, (args.old_container, args.new_container, args.check_container))
    if old["State"]["Status"] != "running" or new["State"]["Status"] not in ("created", "exited"):
        raise RuntimeError("old must be running and candidate must be stopped")
    if argument(old, "-release") != args.old_release or argument(new, "-release") != args.new_release:
        raise RuntimeError("container release mismatch")
    if argument(old, "-state-lock") != argument(new, "-state-lock"):
        raise RuntimeError("state lock paths differ")
    if argument(old, "-control") == argument(new, "-control"):
        raise RuntimeError("control socket paths must differ")
    mounts = lambda x: sorted((m["Source"], m["Destination"], m["RW"]) for m in x["Mounts"])
    if mounts(old) != mounts(new):
        raise RuntimeError("persistent mounts differ; prepare equivalent mounts before release")
    if new["Image"] != check["Image"] or "-preflight" not in (check["Config"].get("Cmd") or []):
        raise RuntimeError("preflight must use candidate image with -preflight")
    if old["HostConfig"]["RestartPolicy"]["Name"] not in ("no", ""):
        raise RuntimeError("old restart policy must be no to prevent reboot reactivating it")
    if new["HostConfig"]["RestartPolicy"]["Name"] not in ("no", ""):
        raise RuntimeError("candidate restart policy must be no during handoff")
    if check["State"]["Running"]:
        raise RuntimeError("preflight container must be stopped")
    if check["Config"].get("Env") != new["Config"].get("Env"):
        raise RuntimeError("preflight environment differs from candidate")
    # Read-only preflight may differ only in mount write permissions.
    locations = lambda x: sorted((m["Source"], m["Destination"]) for m in x["Mounts"])
    if locations(check) != locations(new):
        raise RuntimeError("preflight must inspect the candidate's mounts")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ("old-container", "new-container", "check-container", "old-release", "new-release",
                 "old-control", "new-control", "gateway-control", "new-target", "journal"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--drain-seconds", type=float, default=20)
    p.add_argument("--start-seconds", type=float, default=20)
    args = p.parse_args()
    if args.old_release == args.new_release or args.drain_seconds <= 0 or args.start_seconds <= 0:
        p.error("distinct releases and positive budgets required")
    os.umask(0o077)
    with open(args.journal + ".lock", "a", encoding="utf-8") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return release(args)


def release(args):
    if os.path.exists(args.journal):
        raise RuntimeError("journal already exists; inspect recorded stage before recovery, use a fresh journal for a new attempt")
    phase = "prepare"

    def record(value):
        nonlocal phase
        phase = value
        with open(args.journal + ".tmp", "w", encoding="utf-8") as f:
            json.dump({"phase": value, "old": args.old_release, "new": args.new_release, "at": time.time()}, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(args.journal + ".tmp", args.journal)
        print(value, flush=True)

    def wait_for(predicate, timeout):
        end = time.monotonic() + timeout
        while True:
            if predicate():
                return
            if time.monotonic() >= end:
                raise TimeoutError("release stage exceeded budget")
            time.sleep(0.1)

    validate_containers(args)
    gateway = call(args.gateway_control)
    if gateway["release"] != args.old_release or gateway["held"]:
        raise RuntimeError("gateway release/phase mismatch")
    if gateway["wait_seconds"] < args.drain_seconds + args.start_seconds + 15:
        raise RuntimeError("gateway wait must cover drain + startup + 15 seconds commit/exit margin; check client/proxy deadlines too")
    target = urllib.parse.urlsplit(args.new_target)
    if target.scheme != "http" or target.hostname not in ("127.0.0.1", "::1") or not target.port or target.path or target.query or target.fragment or target.username:
        raise RuntimeError("new target must be an explicit loopback HTTP origin")
    old_status = call(args.old_control)
    if old_status["release"] != args.old_release:
        raise RuntimeError("old control identity mismatch")
    if old_status.get("paused") or old_status.get("background_paused") or not old_status["ready"]:
        raise RuntimeError("old release must be serving before starting a new handoff")
    record("preflight")
    docker("start", args.check_container)
    wait_for(lambda: not inspect(args.check_container)["State"]["Running"], args.start_seconds)
    if inspect(args.check_container)["State"]["ExitCode"] != 0:
        raise RuntimeError("candidate preflight failed; old release unchanged")

    old_stopped = False
    try:
        record("hold")
        call(args.gateway_control, "hold", expected=args.old_release)
        drain_end = time.monotonic() + args.drain_seconds
        def drain_wait(predicate):
            wait_for(predicate, max(0, drain_end - time.monotonic()))
        # Allow already admitted requests (including late backend dial) to enter
        # A before closing its direct admission barrier.
        drain_wait(lambda: call(args.gateway_control)["active"] == 0)
        call(args.old_control, "pause", release=args.old_release)
        drain_wait(lambda: call(args.old_control)["active"] == 0)
        call(args.old_control, "quiesce", release=args.old_release)
        record("drain")
        drain_wait(lambda: call(args.old_control)["background"] == 0)
        record("finalize")
        call(args.old_control, "stop", release=args.old_release)
        # The stop acknowledgment commits us to waiting for actual exit.
        old_stopped = True
        record("old_stopping")
        wait_for(lambda: not inspect(args.old_container)["State"]["Running"], args.start_seconds)
        if inspect(args.old_container)["State"]["ExitCode"] != 0:
            raise RuntimeError("old release did not exit cleanly")
        record("activate")
        docker("start", args.new_container)

        def ready():
            if not inspect(args.new_container)["State"]["Running"]:
                raise RuntimeError("candidate exited; inspect protected logs")
            try:
                state = call(args.new_control)
                if state["release"] != args.new_release or not state["ready"]:
                    return False
                # Verify the actual target too, not just its separate control socket.
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                with opener.open(args.new_target + "/readyz", timeout=2) as response:
                    return json.load(response)["release"] == args.new_release
            except (OSError, http.client.HTTPException):
                return False

        wait_for(ready, args.start_seconds)
        record("switch")
        call(args.gateway_control, "switch", expected=args.old_release, next=args.new_release, target=args.new_target)
        call(args.gateway_control, "resume", expected=args.new_release)
        record("serving")
    except BaseException:
        if not old_stopped and phase != "finalize":
            # No external work is replayed. A still owns its original state.
            call(args.old_control, "resume", release=args.old_release)
            call(args.gateway_control, "resume", expected=args.old_release)
            record("aborted_old_serving")
        else:
            # A failed/lost stop reply is ambiguous: never restart either writer
            # or restore a database snapshot automatically.
            record("recovery_required")
        raise


if __name__ == "__main__":
    main()
