#!/usr/bin/env python3
"""Verify the committed repository from a fresh clone and isolated Compose stack."""

import os
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def run(args, *, cwd, env, timeout=900, capture=True):
    result = subprocess.run(args, cwd=cwd, env=env, text=True, stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.STDOUT if capture else None, timeout=timeout)
    if result.returncode and capture:
        print(f"FAILED: {' '.join(args)}\n{result.stdout[-6000:]}", file=sys.stderr, flush=True)
    return result


def wait_ready(url, seconds=90):
    deadline = time.monotonic() + seconds
    last = None
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url + "/readyz", timeout=2) as response:
                if response.status == 200:
                    return True, response.status
                last = response.status
        except urllib.error.HTTPError as exc:
            last = exc.code
        except Exception as exc:
            last = repr(exc)
        time.sleep(0.5)
    return False, last


def main():
    env_example = ROOT / ".env.example"
    if not env_example.is_file():
        print("FAIL: .env.example missing from committed repository", file=sys.stderr)
        return 1
    suffix = secrets.token_hex(4)
    project = "cleanclone" + suffix
    app_port = free_port()
    db_port = free_port()
    secret = secrets.token_hex(32)
    db_password = "db" + secrets.token_hex(16)
    admin_password = "admin" + secrets.token_hex(16)
    root_password = "root" + secrets.token_hex(16)
    with tempfile.TemporaryDirectory(prefix="ticket-reservation-clean-clone-") as temp:
        clone = Path(temp) / "checkout"
        cloned = run(["git", "clone", "--quiet", "--no-hardlinks", str(ROOT), str(clone)], cwd=ROOT, env=os.environ.copy(), timeout=120)
        if cloned.returncode:
            return 1
        print("PASS: cloned current committed HEAD into a temporary directory", flush=True)

        values = {
            "JWT_SECRET": secret,
            "ADMIN_USERNAME": "admin",
            "ADMIN_PASSWORD": admin_password,
            "DB_PASSWORD": db_password,
            "MARIADB_ROOT_PASSWORD": root_password,
            "PORT": str(app_port),
            "DB_HOST_PORT": str(db_port),
            "DB_HOST": f"127.0.0.1:{db_port}",
            "DB_USER": "ticket",
            "DB_NAME": "tickets",
        }
        env_text = []
        for line in (clone / ".env.example").read_text(encoding="utf-8").splitlines():
            key = line.split("=", 1)[0] if "=" in line and not line.lstrip().startswith("#") else None
            env_text.append(f"{key}={values[key]}" if key in values else line)
        (clone / ".env").write_text("\n".join(env_text) + "\n", encoding="utf-8")
        child_env = os.environ.copy()
        child_env.update(values)
        child_env["BASE_URL"] = f"http://localhost:{app_port}"
        child_env["TEST_DB_DSN"] = f"ticket:{db_password}@tcp(127.0.0.1:{db_port})/tickets?parseTime=true&charset=utf8mb4"
        child_env["COMPOSE_PROJECT_NAME"] = project
        compose = ["docker", "compose", "-p", project]
        stack_started = False
        try:
            up = run([*compose, "up", "--build", "-d"], cwd=clone, env=child_env, timeout=600)
            if up.returncode:
                return 1
            stack_started = True
            print("PASS: docker compose up --build -d", flush=True)
            ready, last = wait_ready(child_env["BASE_URL"])
            if not ready:
                print(f"FAIL: /readyz did not reach 200 within 90s (last={last})", file=sys.stderr)
                return 1
            print("PASS: cloned app reached /readyz=200", flush=True)

            tests = run(["go", "test", "./..."], cwd=clone, env=child_env, timeout=900)
            if tests.returncode:
                return 1
            print(tests.stdout.rstrip(), flush=True)
            print("PASS: go test ./...", flush=True)

            burst = run(["python3", "scripts/burst.py", "--quick"], cwd=clone, env=child_env, timeout=600)
            if burst.returncode:
                return 1
            print(burst.stdout.rstrip(), flush=True)
            print("PASS: short burst", flush=True)
        finally:
            if stack_started:
                logs = run([*compose, "logs", "--no-color", "--tail=250"], cwd=clone, env=child_env, timeout=60)
                if logs.returncode == 0:
                    diagnostic = [line for line in logs.stdout.splitlines() if any(term in line.lower() for term in ("deadlock", "lock wait", "panic", "error"))]
                    if diagnostic:
                        print("Relevant app/DB log lines:", file=sys.stderr)
                        print("\n".join(diagnostic[-80:]), file=sys.stderr)
                down = run([*compose, "down", "--volumes", "--remove-orphans"], cwd=clone, env=child_env, timeout=120)
                if down.returncode:
                    print("FAIL: Compose clean-clone teardown failed", file=sys.stderr)
                    raise RuntimeError("clean clone teardown failed")
                print("PASS: clean-clone Compose stack torn down", flush=True)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"FAIL: clean-clone verification: {exc!r}", file=sys.stderr)
        raise SystemExit(1)
