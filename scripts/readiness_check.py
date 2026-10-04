#!/usr/bin/env python3
"""Exercise readiness fail-closed and recovery against local Docker Compose."""

import os
import subprocess
import sys
import time
import urllib.error
import urllib.request


def compose(*args, timeout=180):
    env = os.environ.copy()
    if not env.get("JWT_SECRET"):
        env["JWT_SECRET"] = "readiness-local-only-secret"
    if not env.get("ADMIN_PASSWORD"):
        env["ADMIN_PASSWORD"] = "readiness-local-only-password"
    return subprocess.run(["docker", "compose", *args], env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)


def check(label, ok, details=""):
    print(f"{'PASS' if ok else 'FAIL'}: {label}" + (f" — {details}" if details else ""), flush=True)
    return ok


def status(path, timeout=2):
    try:
        with urllib.request.urlopen(f"http://localhost:8080{path}", timeout=timeout) as response:
            return response.status
    except urllib.error.HTTPError as exc:
        return exc.code
    except Exception as exc:
        return None


def wait_status(path, expected, duration):
    deadline = time.monotonic() + duration
    last = None
    while time.monotonic() < deadline:
        last = status(path)
        if last == expected:
            return True, last
        time.sleep(0.25)
    return False, last


def main():
    failed = False
    db_stopped = False
    try:
        up = compose("up", "--build", "-d")
        if up.returncode:
            check("Compose services start", False, up.stdout[-1500:])
            return 1
        ready, ready_code = wait_status("/readyz", 200, 60)
        live, live_code = wait_status("/livez", 200, 10)
        failed |= not check("initial /readyz returns 200", ready, f"last status={ready_code}")
        failed |= not check("initial /livez returns 200", live, f"last status={live_code}")
        before = compose("ps", "-q", "app")
        before_id = before.stdout.strip()

        stopped = compose("stop", "db", timeout=60)
        if stopped.returncode:
            failed |= not check("stop Compose database", False, stopped.stdout[-1500:])
            return 1
        db_stopped = True
        ready_down, ready_down_code = wait_status("/readyz", 503, 10)
        failed |= not check("/readyz fails closed within 10s while DB is stopped", ready_down, f"last status={ready_down_code}")
        live_up = status("/livez") == 200
        failed |= not check("/livez stays 200 while DB is stopped", live_up, f"status={status('/livez')}")

        started = compose("start", "db", timeout=60)
        if started.returncode:
            failed |= not check("restart Compose database", False, started.stdout[-1500:])
            return 1
        db_stopped = False
        recovered, recovered_code = wait_status("/readyz", 200, 60)
        failed |= not check("/readyz returns 200 within 60s after DB starts", recovered, f"last status={recovered_code}")
        after = compose("ps", "-q", "app")
        same_app = before_id != "" and before_id == after.stdout.strip()
        failed |= not check("app container was not restarted", same_app, f"before={before_id[:12]} after={after.stdout.strip()[:12]}")
    except Exception as exc:
        failed = True
        check("readiness check completed", False, repr(exc))
    finally:
        if db_stopped:
            restore = compose("start", "db", timeout=60)
            if restore.returncode:
                check("database cleanup/restart", False, restore.stdout[-1500:])
                failed = True
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
