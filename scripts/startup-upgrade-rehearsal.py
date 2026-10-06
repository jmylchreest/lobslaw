#!/usr/bin/env python3
"""Run real historical installs, then upgrade their actual encrypted data.

Build the binaries and upgrade-probe from the pinned historical worktrees first.
The probe must be compiled against each historical module, not just the latest
protobuf. Supply binary paths explicitly. All installs, credentials, ports and
HOME directories are isolated. Evidence is retained in a private output folder.
The only simulated component is the external OpenAI-compatible LLM/embedding API.
"""

import argparse
import base64
import hashlib
import hmac
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def save(path, value):
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n")
    path.chmod(0o600)


def sha(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def core_files(directory):
    return {str(p.relative_to(directory)): sha(p) for p in directory.rglob("*")
            if p.is_file() and (p.name in {"state.db", "raft.db", "data-migration.json"}
                               or "snapshots" in p.relative_to(directory).parts)}


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_for(description, predicate, timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.25)
    raise AssertionError("timed out: " + description)


class Provider(BaseHTTPRequestHandler):
    requests = []
    lock = threading.Lock()

    def log_message(self, *_):
        pass

    def do_GET(self):
        self.respond({"data": [{"id": "rehearsal-chat"}]})

    def respond(self, data):
        body = json.dumps(data).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with self.lock:
            self.requests.append({"path": self.path, "body": body})
        if self.path.endswith("/embeddings"):
            inputs = body.get("input", [])
            if isinstance(inputs, str):
                inputs = [inputs]
            self.respond({"object": "list", "model": "rehearsal-embedding", "data": [
                {"object": "embedding", "index": i, "embedding": [1, 0.25, 0.5, -0.25, 0, 0.125, 0.75, -1]}
                for i, _ in enumerate(inputs)], "usage": {"prompt_tokens": 10, "total_tokens": 10}})
            return
        text = "Rehearsal reply: your historical conversation is intact. café 日本語 🦞"
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            chunks = [{"choices": [{"index": 0, "delta": {"role": "assistant", "content": text}, "finish_reason": None}]},
                      {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 10, "completion_tokens": 10}}]
            for chunk in chunks:
                self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        else:
            self.respond({"id": "rehearsal", "model": "rehearsal-chat", "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})


class Install:
    def __init__(self, root, provider, binaries, name, container_image=None):
        self.root = root / name
        self.root.mkdir(mode=0o700)
        self.data = self.root / "data"
        self.data.mkdir(mode=0o700)
        for name in ("certs", "home", "incoming", "audit", "workspace"):
            (self.root / name).mkdir(mode=0o700)
        self.cluster, self.gateway, self.enrol = port(), port(), port()
        self.binaries = binaries
        self.container_image = container_image
        self.container_name = None
        self.node = None
        self.env = {k: v for k, v in os.environ.items() if not k.startswith("LOBSLAW")}
        self.env.update(HOME=str(self.root / "home"), XDG_CONFIG_HOME=str(self.root / "home/.config"),
                        LOBSLAW_MEMORY_KEY=base64.b64encode(secrets.token_bytes(32)).decode(),
                        UPGRADE_JWT_SECRET=secrets.token_hex(32), REHEARSAL_API_KEY="synthetic-local-provider")
        save(self.root / "test-secrets.json", {k: self.env[k] for k in ("LOBSLAW_MEMORY_KEY", "UPGRADE_JWT_SECRET")})
        self.config = self.root / "config.toml"
        self.config.write_text(f'''[cluster]
listen_addr = "127.0.0.1:{self.cluster}"
advertise_addr = "127.0.0.1:{self.cluster}"
data_dir = "{self.data}"
bootstrap = true
[cluster.mtls]
ca_cert = "{self.root}/certs/ca.pem"
node_cert = "{self.root}/certs/node.pem"
node_key = "{self.root}/certs/node-key.pem"
operator_ca_cert = "{self.data}/operator-ca.pem"
operator_ca_key = "{self.data}/operator-ca-key.pem"
enrol_addr = "127.0.0.1:{self.enrol}"
[memory]
enabled = true
[memory.encryption]
key_ref = "env:LOBSLAW_MEMORY_KEY"
[memory.snapshot]
target = "storage:local-snapshots"
[storage]
enabled = true
[[storage.mounts]]
label = "local-snapshots"
type = "local"
path = "{self.data}/snapshots"
[[storage.mounts]]
label = "workspace"
type = "local"
path = "{self.root}/workspace"
mode = "rw"
[policy]
enabled = true
[compute]
enabled = true
artifact_mount = "workspace"
[compute-teams]
enabled = true
[[compute.providers]]
label = "rehearsal"
endpoint = "http://127.0.0.1:{provider}/v1"
model = "rehearsal-chat"
api_key_ref = "env:REHEARSAL_API_KEY"
trust_tier = "private"
capabilities = ["chat", "function-calling"]
auto_capabilities = false
[compute.roles]
main = "rehearsal"
[compute.embeddings]
endpoint = "http://127.0.0.1:{provider}/v1"
model = "rehearsal-embedding"
api_key_ref = "env:REHEARSAL_API_KEY"
dims = 8
[gateway]
enabled = true
http_port = {self.gateway}
incoming_dir = "{self.root}/incoming"
[auth]
allow_hs256 = true
jwt_secret_ref = "env:UPGRADE_JWT_SECRET"
require_auth = true
[security]
egress_allow_private_ranges = true
egress_allow_ranges = ["127.0.0.1/32"]
[trace]
enabled = true
[audit.local]
enabled = true
path = "{self.root}/audit/audit.jsonl"
[[user]]
id = "alice"
display_name = "Upgrade Alice"
timezone = "Europe/London"
roles = ["operator"]
[[user.channels]]
type = "rest"
address = "alice"
''')
        self.config.chmod(0o600)
        header = self.b64({"alg": "HS256", "typ": "JWT"})
        payload = self.b64({"sub": "alice", "scope": "owner", "roles": ["operator"], "iat": int(time.time()), "exp": int(time.time()) + 7200})
        raw = header + "." + payload
        signature = base64.urlsafe_b64encode(hmac.new(self.env["UPGRADE_JWT_SECRET"].encode(), raw.encode(), hashlib.sha256).digest()).decode().rstrip("=")
        self.jwt = raw + "." + signature
        self.cookies = http.cookiejar.CookieJar()
        self.http = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.cookies))
        self.node_id = self.cmd(binaries["old"], "nodeid").strip()
        self.cmd(binaries["old"], "cluster", "ca-init", "--ca-cert", str(self.root / "certs/ca.pem"), "--ca-key", str(self.root / "certs/ca-key.pem"))
        self.cmd(binaries["old"], "cluster", "sign-node", "--ca-cert", str(self.root / "certs/ca.pem"), "--ca-key", str(self.root / "certs/ca-key.pem"), "--node-cert", str(self.root / "certs/node.pem"), "--node-key", str(self.root / "certs/node-key.pem"), "--node-id", self.node_id, "--ip", "127.0.0.1", "--dns", "localhost")

    @staticmethod
    def b64(value):
        return base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip("=")

    def cmd(self, binary, *args, expected=0, env=None):
        result = subprocess.run([str(binary), *args], cwd=self.root, env=env or self.env,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=300)
        check(result.returncode == expected, f"command failed ({result.returncode}): {binary} {' '.join(args)}\n{result.stderr}")
        return result.stdout

    def probe(self, mode, prefix="before", churn=0):
        binary = self.binaries["old_probe"] if self.phase == "old" else self.binaries["probe"]
        result = json.loads(self.cmd(binary, "--mode", mode, "--prefix", prefix, "--churn", str(churn), "--addr", f"127.0.0.1:{self.cluster}", "--cert-dir", str(self.root / "certs")))
        save(self.root / f"{self.phase}-{mode}-{prefix}.json", result)
        return result

    def dump(self, label, directory=None):
        result = json.loads(self.cmd(self.binaries["probe"], "--mode", "dump", "--state", str((directory or self.data) / "state.db")))
        save(self.root / f"{label}-state-fingerprints.json", result)
        return result

    def request(self, path, body=None, bearer=False, expected=200):
        headers = {"Content-Type": "application/json", "Accept": "application/json", "Origin": f"http://127.0.0.1:{self.gateway}"}
        if bearer:
            headers["Authorization"] = "Bearer " + self.jwt
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(f"http://127.0.0.1:{self.gateway}{path}", data=data, headers=headers)
        try:
            response = self.http.open(request, timeout=90)
        except urllib.error.HTTPError as err:
            check(err.code == expected, f"{path}: HTTP {err.code}: {err.read().decode()}")
            return None
        with response:
            raw = response.read()
            check(response.status == expected, f"{path}: HTTP {response.status}: {raw}")
            return json.loads(raw)

    def start(self, phase, label):
        check(self.node is None, "already running")
        self.phase = phase
        log = (self.root / f"{label}.log").open("w")
        self.node = subprocess.Popen(self.node_command(phase, self.config), cwd=self.root, env=self.env, stdout=log, stderr=log)
        log.close()
        def ready():
            check(self.node.poll() is None, f"node exited during {label}; see {self.root / (label + '.log')}")
            try:
                self.request("/readyz")
                return True
            except (OSError, AssertionError):
                return False
        wait_for(label + " readiness", ready)
        # /readyz alone does not prove the Raft leader can commit.
        time.sleep(2)

    def node_command(self, phase, config):
        if not self.container_image:
            return [str(self.binaries[phase]), "run", "--config", str(config)]
        self.container_name = "lobslaw-upgrade-rehearsal-" + secrets.token_hex(6)
        return ["docker", "run", "--rm", "--name", self.container_name,
                "--network", "host", "--hostname", socket.gethostname(), "--user", f"{os.getuid()}:{os.getgid()}",
                "--workdir", str(self.root), "--mount", f"type=bind,src={self.root},dst={self.root}",
                "--mount", f"type=bind,src={self.binaries[phase]},dst=/usr/local/bin/lobslaw,readonly",
                *[arg for name in ("HOME", "XDG_CONFIG_HOME", "LOBSLAW_MEMORY_KEY", "UPGRADE_JWT_SECRET", "REHEARSAL_API_KEY") for arg in ("--env", name)],
                self.container_image, "/usr/local/bin/lobslaw", "run", "--config", str(config)]

    def cleanup_container(self):
        if self.container_name:
            subprocess.run(["docker", "rm", "--force", self.container_name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            self.container_name = None

    def stop(self):
        if self.node:
            self.node.send_signal(signal.SIGTERM)
            try:
                self.node.wait(timeout=20)
            except subprocess.TimeoutExpired:
                self.node.kill()
                self.node.wait()
                raise AssertionError("node failed graceful shutdown")
            finally:
                self.node = None
                self.cleanup_container()

    def reject(self, label, env=None, directory=None):
        directory = directory or self.data
        before = core_files(directory)
        cfg = self.config
        if directory != self.data:
            cfg = self.root / (label + "-config.toml")
            cfg.write_text(self.config.read_text().replace(str(self.data), str(directory)))
        try:
            result = subprocess.run(self.node_command("candidate", cfg), cwd=self.root,
                                    env=env or self.env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=45)
        except subprocess.TimeoutExpired as err:
            (self.root / (label + ".log")).write_bytes(err.stdout or b"")
            raise AssertionError(label + " did not refuse startup") from err
        finally:
            self.cleanup_container()
        (self.root / (label + ".log")).write_text(result.stdout)
        check(result.returncode != 0, label + " unexpectedly booted")
        check(before == core_files(directory), label + " modified persisted core data")
        return result.stdout

    def chat(self, marker, session="upgrade-thread"):
        response = self.request("/v1/messages", {"message": marker, "session_id": session}, bearer=True)
        check("Rehearsal reply" in response.get("reply", ""), "chat did not run the LLM")
        save(self.root / f"chat-{marker.split()[0]}.json", response)
        return response

    def transcript(self):
        response = json.loads(self.cmd(self.binaries[self.phase], "session", "show", "rest:alice/upgrade-thread", "--json", "--addr", f"127.0.0.1:{self.cluster}", "--ca-cert", str(self.root / "certs/ca.pem"), "--node-cert", str(self.root / "certs/node.pem"), "--node-key", str(self.root / "certs/node-key.pem")))
        save(self.root / f"{self.phase}-transcript.json", response)
        return response


PRESERVED = ("vector_records", "episodic_records", "sessions", "session_messages", "pinned", "credentials", "commitments", "scheduled_tasks", "user_prefs", "integration_state")


def assert_preserved(before, after, exact=True, allow_housekeeping=False):
    count = 0
    for bucket in PRESERVED:
        for key, value in before.get(bucket, {}).items():
            if allow_housekeeping and bucket == "scheduled_tasks" and key.startswith("lobslaw-builtin-"):
                continue  # A running scheduler legitimately advances these.
            check(after.get(bucket, {}).get(key) == value, f"changed or lost {bucket}/{key}")
            count += 1
        if exact:
            check(before.get(bucket, {}) == after.get(bucket, {}), f"unexpected migration changes in {bucket}")
    return count


def populate(install, snapshots, shared=False):
    install.start("old", "old-first-boot")
    install.probe("seed", churn=9000 if snapshots else 0)
    for i in range(4):
        install.chat(f"OLD-TURN-{i} historical memory café 日本語 🦞")
    if shared:
        out = install.cmd(install.binaries["old"], "memory", "share", "before-vector-00", "--apply", "--json", "--addr", f"127.0.0.1:{install.cluster}", "--ca-cert", str(install.root / "certs/ca.pem"), "--node-cert", str(install.root / "certs/node.pem"), "--node-key", str(install.root / "certs/node-key.pem"))
        (install.root / "old-memory-share.json").write_text(out)
        # Historical main's connector envelope field 38 conflicts with older
        # team builds. Use the actual old protobuf and old node's Raft path.
        install.probe("connector")
    transcript = install.transcript()
    check("OLD-TURN-0" in json.dumps(transcript), "old transcript missing messages")
    if snapshots:
        wait_for("old binary creates an actual Raft snapshot", lambda: bool(list((install.data / "snapshots").rglob("state.bin"))), timeout=270)
    install.stop()
    before = install.dump("old-before-upgrade")
    install.start("old", "old-restart")
    install.probe("verify")
    check(install.transcript() == transcript, "old install did not preserve its own transcript on restart")
    install.stop()
    before = install.dump("old-final")
    save(install.root / "old-core-file-sha256.json", core_files(install.data))
    return before, transcript


def exercise_upgraded(install, before, transcript, recover=False):
    if recover:
        install.config.write_text(install.config.read_text().replace("[memory]\n", "[memory]\nrestore_mode = true\n"))
        # Boot once in recovery mode without re-enabling external work.
        log = (install.root / "candidate-recovery-mode.log").open("w")
        install.node = subprocess.Popen(install.node_command("candidate", install.config), cwd=install.root, env=install.env, stdout=log, stderr=log)
        log.close()
        time.sleep(5)
        check(install.node.poll() is None, "recovery-mode boot failed")
        install.phase = "candidate"
        install.probe("verify")
        install.stop()
        install.cmd(install.binaries["candidate"], "data", "accept-recovery", "--data-dir", str(install.data), "--memory-key-ref", "env:LOBSLAW_MEMORY_KEY", "--acknowledge-external-effects")
        install.config.write_text(install.config.read_text().replace("restore_mode = true\n", ""))
    install.start("candidate", "candidate-first-boot")
    install.probe("verify")
    check(install.transcript() == transcript, "upgrade changed historical transcript")
    install.request("/v1/session", {}, bearer=True)
    install.stop()
    upgraded = install.dump("candidate-after-upgrade")
    preserved = assert_preserved(before, upgraded)
    install.start("candidate", "candidate-post-upgrade-use")
    install.request("/v1/session")
    def activated():
        status = json.loads(install.cmd(install.binaries["candidate"], "cluster", "upgrade", "status", "--addr", f"127.0.0.1:{install.cluster}", "--ca-cert", str(install.root / "certs/ca.pem"), "--node-cert", str(install.root / "certs/node.pem"), "--node-key", str(install.root / "certs/node-key.pem")))
        save(install.root / "automatic-upgrade-status.json", status)
        return status["active_contract"] == 2
    wait_for("real automatic contract 1 to 2 activation", activated, timeout=120)
    install.probe("verify")
    install.request("/v1/bots", bearer=True)
    created = install.request("/v1/bots", {"id": "rehearsal-bot", "name": "Upgrade rehearsal bot", "instructions": "Preserve this bot across restarts", "model": "rehearsal"}, bearer=True, expected=201)
    save(install.root / "created-bot.json", created)
    request_index = len(Provider.requests)
    install.chat("NEW-TURN-0 continue our historical conversation")
    with Provider.lock:
        calls = json.dumps(Provider.requests[request_index:])
    check("OLD-TURN-0" in calls, "new turn did not replay historical context into LLM")
    check("UPGRADE-PINNED" in calls, "new turn did not inject preserved pinned profile")
    install.probe("seed", prefix="after")
    after_transcript = install.transcript()
    check("NEW-TURN-0" in json.dumps(after_transcript), "new transcript write missing")
    install.stop()
    after = install.dump("candidate-after-new-writes")
    # The conversation index advances; the old transcript messages stay byte-exact.
    for bucket in ("sessions", "pinned"):
        before = {k: v for k, v in before.items() if k != bucket}
    assert_preserved(before, after, exact=False, allow_housekeeping=True)
    install.start("candidate", "candidate-second-restart")
    install.request("/v1/session")
    install.probe("verify")
    install.probe("verify", prefix="after")
    bot = install.request("/v1/bots/rehearsal-bot", bearer=True)
    check(bot["id"] == "rehearsal-bot", "activated feature data lost on restart")
    check(install.transcript() == after_transcript, "new writes disappeared or duplicated on restart")
    install.chat("NEW-TURN-1 after a second restart")
    install.stop()
    final = install.dump("candidate-final")
    return {"preserved_records": preserved, "final_counts": {k: len(v) for k, v in final.items()},
            "state_format": final.get("data_format"), "snapshots": len(list((install.data / "snapshots").rglob("state.bin"))), "automatic_activation": "contract 1 to 2, bot creation/readback available without another restart", "browser_cookie_restart": "passed"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("legacy", "versioned", "candidate", "legacy-probe", "versioned-probe", "probe", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--snapshots", action="store_true", help="drive 9000 historical writes and wait for real snapshots")
    parser.add_argument("--scenario", choices=("direct", "ambiguous", "versioned"), action="append", help="run only selected scenarios (repeatable)")
    parser.add_argument("--container-image", help="run both old and new nodes in recreated non-root Docker containers")
    args = parser.parse_args()
    root = args.output.resolve()
    check(not root.exists(), "output must be a new isolated directory")
    root.mkdir(mode=0o700)
    os.umask(0o077)
    provider = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    thread = threading.Thread(target=provider.serve_forever, daemon=True)
    thread.start()
    installs = []
    report = {"result": "in-progress", "scenarios": {}, "mocked": "external LLM and embeddings only", "runtime": args.container_image or "native real binaries"}
    save(root / "report.json", report)
    try:
        for label, old, old_probe in (("unversioned-direct-5f4e6c4", args.legacy, args.legacy_probe), ("unversioned-sharing-5f4e6c4", args.legacy, args.legacy_probe), ("versioned-4b1179a", args.versioned, args.versioned_probe)):
            scenario = "direct" if "direct" in label else "ambiguous" if "sharing" in label else "versioned"
            if args.scenario and scenario not in args.scenario:
                continue
            print(f"Running {label}", flush=True)
            install = Install(root, provider.server_port, {"old": old.resolve(), "old_probe": old_probe.resolve(), "candidate": args.candidate.resolve(), "probe": args.probe.resolve()}, label, args.container_image)
            installs.append(install)
            before, transcript = populate(install, args.snapshots, shared="sharing" in label)
            unchanged_source = core_files(install.data)
            if "direct" in label:
                wrong_env = dict(install.env, LOBSLAW_MEMORY_KEY=base64.b64encode(secrets.token_bytes(32)).decode())
                install.reject("wrong-key-refusal", env=wrong_env)
                result = exercise_upgraded(install, before, transcript)
                backups = list(install.data.glob("startup-backup-*"))
                check(len(backups) == 1, "startup did not create exactly one backup across restarts")
                backup = backups[0]
                check(core_files(backup) == unchanged_source, "startup backup is not byte-identical to the original core files")
                install.reject("startup-backup-not-a-live-node", directory=backup)
                # Verify rollback with the real old binary against the startup
                # backup, not just by inspecting a synthesized fixture.
                source = install.data
                for auxiliary in ("operator-ca.pem", "operator-ca-key.pem"):
                    shutil.copy2(source / auxiliary, backup / auxiliary)
                install.data = backup
                install.config.write_text(install.config.read_text().replace(str(source), str(backup)))
                install.start("old", "original-binary-startup-backup-rollback")
                install.probe("verify")
                check(install.transcript() == transcript, "startup-backup rollback changed historical transcript")
                install.stop()
                result.update(direct_startup="passed automatic unversioned migration on the same install", backup="one byte-identical original core image, no repeat backup on restart", rollback="passed with the old binary against startup backup", wrong_key="refused without core data changes")
            elif "sharing" in label:
                refusal = install.reject("ambiguous-startup-refusal")
                check("legacy" in refusal.lower(), "unexpected startup refusal")
                wrong_env = dict(install.env, LOBSLAW_MEMORY_KEY=base64.b64encode(secrets.token_bytes(32)).decode())
                install.reject("wrong-key-refusal", env=wrong_env)
                source = install.data
                destination = install.root / "migrated"
                inspection = install.cmd(args.candidate, "data", "inspect", "--data-dir", str(source), "--memory-key-ref", "env:LOBSLAW_MEMORY_KEY", "--legacy-format", "main-v0")
                (install.root / "inspection.json").write_text(inspection)
                migration = install.cmd(args.candidate, "data", "migrate", "--data-dir", str(source), "--output", str(destination), "--memory-key-ref", "env:LOBSLAW_MEMORY_KEY", "--legacy-format", "main-v0")
                (install.root / "migration.json").write_text(migration)
                check(unchanged_source == core_files(source), "offline migration changed original install")
                check(sha(source / "raft.db") == sha(destination / "raft.db"), "migration rewrote historical Raft bytes")
                check(core_files(source).keys() - {"state.db"} <= core_files(destination).keys(), "migration lost snapshots/logs")
                # Recovery images exclude auxiliary installation files. Preserve
                # the same operator identity/browser sessions explicitly.
                for auxiliary in ("operator-ca.pem", "operator-ca-key.pem", "auth"):
                    path = source / auxiliary
                    if path.is_dir():
                        shutil.copytree(path, destination / auxiliary)
                    elif path.exists():
                        shutil.copy2(path, destination / auxiliary)
                install.data = destination
                install.config.write_text(install.config.read_text().replace(str(source), str(destination)))
                install.reject("recovery-acknowledgement-required")
                result = exercise_upgraded(install, before, transcript, recover=True)
                check(unchanged_source == core_files(source), "source changed while upgraded copy ran")
                # Roll back the untouched original using the exact historical
                # binary/keys/identity, after stopping the upgraded copy.
                install.data = source
                install.config.write_text(install.config.read_text().replace(str(destination), str(source)))
                install.start("old", "original-binary-rollback")
                install.probe("verify")
                check(install.transcript() == transcript, "rollback original lost its transcript")
                install.stop()
                result.update(direct_startup="refused ambiguous provenance without data writes", offline_migration="passed with verified main-v0 provenance", rollback="passed on untouched original", wrong_key="refused without core data changes")
            else:
                check(before.get("data_format", {}).get("manifest", {}).get("format", {}).get("version") == 1, "versioned baseline did not produce contract 1")
                result = exercise_upgraded(install, before, transcript)
                result["direct_startup"] = "passed against the same data directory"
                fault = install.root / "unsupported-future-data"
                shutil.copytree(install.data, fault)
                install.cmd(args.probe, "--mode", "future-state", "--state", str(fault / "state.db"))
                install.reject("future-format-refusal", directory=fault)
                result["future_format"] = "refused without core data changes"
            report["scenarios"][label] = result
            save(root / "report.json", report)
        report["result"] = "passed"
        save(root / "report.json", report)
        print(json.dumps(report, indent=2), flush=True)
    except Exception as error:
        report["result"] = "failed"
        report["failure"] = str(error)
        save(root / "report.json", report)
        raise
    finally:
        for install in installs:
            install.stop()
        provider.shutdown()
        with Provider.lock:
            save(root / "provider-requests.json", Provider.requests)
        print(f"Evidence: {root}", flush=True)


if __name__ == "__main__":
    main()
