#!/usr/bin/env python3
"""One host slot supervises one JIT ephemeral container at a time."""
import argparse
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import threading
import time
import uuid

REPOSITORY = "vmxmy/orca-factory"
LABEL = "orca-go-isolated"
DOCKER = "/usr/bin/docker"
GH = "/usr/bin/gh"
STOP = threading.Event()
SLOTS = (1, 2, 3, 4)


def validate_workers(n):
    if n not in SLOTS:
        raise ValueError("only slots 1 through 4 are supported")


def container_args(image, name, owner):
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", image):
        raise ValueError("an immutable local image ID is required")
    return [DOCKER, "run", "--rm", "--init", "-i", "--name", name,
            "--user", "1001:123", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
            "--cpus", "4", "--memory", "8g", "--memory-swap", "8g", "--pids-limit", "512",
            "--network", "bridge", "--log-driver", "none",
            "--tmpfs", "/tmp:rw,nosuid,nodev,exec,size=1g,uid=1001,gid=123",
            "--tmpfs", "/home/runner/_work:rw,nosuid,nodev,exec,size=6g,uid=1001,gid=123",
            "--label", "orca.local-runner.owner=" + owner, image]


def jit_payload(name):
    return {"name": name, "runner_group_id": 1,
            "labels": ["self-hosted", "Linux", "X64", LABEL], "work_folder": "_work"}


def redact(message, jit=""):
    if jit:
        message = message.replace(jit, "[REDACTED]")
    message = re.sub(r"\b(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]+", "[REDACTED]", message)
    message = re.sub(r"(?i)(Bearer\s+)\S+", r"\1[REDACTED]", message)
    if re.search(r"(?i)jitconfig|authorization|credential|token", message):
        return "[sensitive runner diagnostic suppressed]"
    return message[:2000]


def event(kind, **values):
    print(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                      "event": kind, **values}), flush=True)


def owned_container(info, owner):
    return info.get("Config", {}).get("Labels", {}).get("orca.local-runner.owner") == owner


def command(args, data=None, timeout=60):
    return subprocess.run(args, input=data, capture_output=True, text=True, timeout=timeout)


class RegistrationNotSent(RuntimeError):
    """The registration connection failed before an HTTP request could be sent."""


def api(method, suffix, payload=None, missing_ok=False):
    args = [GH, "api", "--method", method, "repos/" + REPOSITORY + "/actions/runners" + suffix,
            "-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2022-11-28"]
    if payload is not None:
        args += ["--input", "-"]
    result = command(args, json.dumps(payload) if payload is not None else None)
    if result.returncode:
        status = re.search(r"HTTP (\d{3})", result.stderr)
        code = status.group(1) if status else "unknown"
        if missing_ok and code == "404":
            return None
        # gh sends stdin as a non-replayable POST body. Only a direct dial
        # failure for the original POST URL proves no registration was sent;
        # redirects, proxy errors, TLS/read timeouts and lost responses do not.
        url = "https://api.github.com/repos/" + REPOSITORY + "/actions/runners/generate-jitconfig"
        if (method == "POST" and suffix == "/generate-jitconfig"
                and re.fullmatch(r'Post "' + re.escape(url) + r'": dial tcp [^\r\n]+', result.stderr.strip())):
            raise RegistrationNotSent("GitHub registration connection failed before sending request")
        raise RuntimeError("GitHub runner API failed: HTTP " + code)
    return json.loads(result.stdout) if result.stdout.strip() else None


def inspect_container(name, owner):
    result = command([DOCKER, "container", "inspect", name])
    if result.returncode:
        if "no such object" in result.stderr.lower() or "no such container" in result.stderr.lower():
            return None
        raise RuntimeError("Docker inspect failed")
    info = json.loads(result.stdout)[0]
    if not owned_container(info, owner):
        raise RuntimeError("container ownership mismatch")
    return info


def runner_busy(lease):
    runner = api("GET", "/" + str(lease["id"]), missing_ok=True)
    if runner is None:
        return False
    if runner["name"] != lease["name"]:
        raise RuntimeError("runner ownership mismatch")
    return bool(runner["busy"])


def finish(lease, owner):
    info = inspect_container(lease["name"], owner)
    if info and info["State"]["Running"]:
        raise RuntimeError("refuse to remove a running container")
    if runner_busy(lease):
        raise RuntimeError("refuse to remove a busy runner")
    api("DELETE", "/" + str(lease["id"]), missing_ok=True)
    if info:
        result = command([DOCKER, "rm", lease["name"]])
        if result.returncode:
            raise RuntimeError("container cleanup failed")
    lease["active"] = False


def save(path, data):
    temporary = path.with_suffix(".next")
    with temporary.open("w") as stream:
        stream.write(json.dumps(data, indent=2) + "\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def resolve_intent(path, lease):
    # A lost POST response must never lead to another unknown registration.
    page = 1
    matches = []
    while True:
        runners = api("GET", f"?per_page=100&page={page}")["runners"]
        matches.extend(runner for runner in runners if runner["name"] == lease["name"])
        if len(runners) < 100:
            break
        page += 1
    if len(matches) != 1:
        # A POST may still be in flight or not yet visible. Retain intent and retry.
        raise RuntimeError("unresolved registration intent; retained for next recovery")
    lease["id"] = matches[0]["id"]
    lease["phase"] = "registered"
    save(path, lease)


def recover(path, owner):
    if not path.exists():
        return
    lease = json.loads(path.read_text())
    if not lease.get("active"):
        return
    if STOP.is_set():
        event("lease_retained", reason="stop before recovery; next start recovers")
        return
    if "id" not in lease:
        resolve_intent(path, lease)
    event("recover_owned_lease", name=lease["name"], runner_id=lease["id"])
    while True:
        if STOP.is_set():
            event("lease_retained", name=lease["name"], reason="stop during recovery; next start recovers")
            return
        info = inspect_container(lease["name"], owner)
        if not info or not info["State"]["Running"]:
            break
        # Even an idle listener can receive a job after an API status check.
        # Never stop a live container during recovery; wait for natural exit.
        STOP.wait(15)
    finish(lease, owner)
    save(path, lease)


def one_cycle(args, owner, path, image):
    name = f"orca-go-{owner}-{args.slot}-{uuid.uuid4().hex[:8]}"
    lease = {"name": name, "active": True, "image": image, "phase": "registering"}
    save(path, lease)
    try:
        response = api("POST", "/generate-jitconfig", jit_payload(name))
    except RegistrationNotSent:
        lease.update(active=False, phase="registration_not_sent")
        save(path, lease)
        event("registration_not_sent", slot=args.slot, name=name)
        raise
    lease.update(id=response["runner"]["id"], phase="registered")
    save(path, lease)
    jit = response["encoded_jit_config"]
    # Fail closed if GitHub returns a persistent runner configuration.
    config = json.loads(base64.b64decode(jit))
    settings = json.loads(base64.b64decode(config[".runner"]))
    if not settings.get("Ephemeral"):
        finish(lease, owner)
        save(path, lease)
        raise RuntimeError("GitHub did not issue an ephemeral JIT configuration")
    # The JIT settings file is runner-local; suppress auto-update to keep the image fixed.
    settings["DisableUpdate"] = True
    config[".runner"] = base64.b64encode(json.dumps(settings).encode()).decode()
    jit = base64.b64encode(json.dumps(config).encode()).decode()
    event("jit_registered", name=name, runner_id=lease["id"], slot=args.slot)
    process = subprocess.Popen(container_args(image, name, owner), stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    def forward():
        for line in process.stdout:
            event("runner_output", slot=args.slot, message=redact(line.strip(), jit))
    reader = threading.Thread(target=forward, daemon=True)
    reader.start()
    try:
        process.stdin.write(jit + "\n")
        process.stdin.close()
        started = time.monotonic()
        while process.poll() is None:
            expired = args.idle_timeout > 0 and time.monotonic() - started >= args.idle_timeout
            if STOP.is_set() or expired:
                STOP.set()
                event("lease_retained", slot=args.slot,
                      reason="supervisor stopped; container left for natural single-job completion")
                return
            STOP.wait(1)
        reader.join(timeout=5)
        event("container_exited", slot=args.slot, code=process.returncode)
    finally:
        if process.poll() is not None:
            finish(lease, owner)
            save(path, lease)
    if process.returncode not in (0, 143) and not STOP.is_set():
        raise RuntimeError("runner container exited unsuccessfully")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-file", type=Path, required=True)
    parser.add_argument("--state-dir", type=Path, required=True)
    parser.add_argument("--slot", type=int, choices=SLOTS, default=1)
    parser.add_argument("--max-cycles", type=int, default=0, help="0 continuously; 1 one ephemeral container")
    parser.add_argument("--idle-timeout", type=int, default=0, help="stop supervisor after N seconds, preserving any live container/lease")
    args = parser.parse_args()
    if args.max_cycles < 0 or args.idle_timeout < 0:
        parser.error("limits must be nonnegative")
    validate_workers(args.slot)
    image = args.image_file.read_text().strip()
    container_args(image, "validation", "validation")
    args.state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    lock = (args.state_dir/f"slot-{args.slot}.lock").open("a")
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    owner = hashlib.sha256((str(args.state_dir.resolve()) + REPOSITORY).encode()).hexdigest()[:10]
    path = args.state_dir/f"slot-{args.slot}.json"
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: STOP.set())
    count = 0
    try:
        recover(path, owner)
        while not STOP.is_set() and (args.max_cycles == 0 or count < args.max_cycles):
            one_cycle(args, owner, path, image)
            count += 1
    except Exception:
        if path.exists() and json.loads(path.read_text()).get("active"):
            event("lease_retained", slot=args.slot, reason="operation failed; next start recovers", path=str(path))
        raise
    event("supervisor_stopped", slot=args.slot, cycles=count)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        event("supervisor_error", error=redact(str(error)) if isinstance(error, RuntimeError) else type(error).__name__)
        raise SystemExit(1)
