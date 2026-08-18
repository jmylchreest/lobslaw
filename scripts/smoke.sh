#!/usr/bin/env bash
# End-to-end smoke test: boot a real node and enrol an operator.
#
# R28 and R29 shipped with green unit tests and did not work. Both
# failures lived in seams — a client verifying its own certificate
# against the wrong root, and a server snapshotting a CA pool before
# the root existed — which no test of the pieces could see. This runs
# the path.
#
# Hermetic: its own temp directory, its own HOME and XDG_CONFIG_HOME,
# loopback only, ports chosen high and checked free. It never reads the
# operator's real configuration, which is how a stray `lobslaw nodeid`
# once booted somebody's live assistant.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
BIN="$WORK/lobslaw"
NODE_PID=""

# Ports are picked once and asserted free. A smoke test that silently
# collides with something already listening reports a failure that has
# nothing to do with the code.
CLUSTER_PORT="${SMOKE_CLUSTER_PORT:-27443}"
ENROL_PORT="${SMOKE_ENROL_PORT:-29091}"
GATEWAY_PORT="${SMOKE_GATEWAY_PORT:-28443}"

cleanup() {
  local rc=$?
  if [ -n "$NODE_PID" ] && kill -0 "$NODE_PID" 2>/dev/null; then
    kill "$NODE_PID" 2>/dev/null || true
    wait "$NODE_PID" 2>/dev/null || true
  fi
  if [ $rc -ne 0 ] && [ -f "$WORK/node.log" ]; then
    echo "--- node log (last 40 lines) ---" >&2
    tail -40 "$WORK/node.log" >&2
  fi
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT

say() { printf '\n=== %s ===\n' "$1"; }
fail() { printf 'SMOKE FAILED: %s\n' "$1" >&2; exit 1; }

for p in "$CLUSTER_PORT" "$ENROL_PORT" "$GATEWAY_PORT"; do
  if ss -ltn 2>/dev/null | grep -q ":$p "; then
    fail "port $p is already in use; set SMOKE_CLUSTER_PORT / SMOKE_ENROL_PORT / SMOKE_GATEWAY_PORT"
  fi
done

say "building"
go build -o "$BIN" "$ROOT/cmd/lobslaw"

export HOME="$WORK/home"
export XDG_CONFIG_HOME="$WORK/home/.config"
mkdir -p "$XDG_CONFIG_HOME/lobslaw" "$WORK/certs" "$WORK/data" "$WORK/laptop" "$WORK/boot"
cd "$WORK"

say "cluster CA and node certificate"
"$BIN" cluster ca-init --ca-cert certs/ca.pem --ca-key certs/ca-key.pem >/dev/null
NODE_ID="$("$BIN" nodeid)"
[ -n "$NODE_ID" ] || fail "nodeid printed nothing"
# --ip is the whole reason this exists: without it the certificate is
# valid only for a hostname equal to the node id, and every documented
# `--addr host:port` fails the handshake.
"$BIN" cluster sign-node --ca-cert certs/ca.pem --ca-key certs/ca-key.pem \
  --node-cert certs/node.pem --node-key certs/node-key.pem \
  --node-id "$NODE_ID" --ip 127.0.0.1 --dns localhost >/dev/null

cat > config.toml <<EOF
[cluster]
listen_addr    = "127.0.0.1:$CLUSTER_PORT"
advertise_addr = "127.0.0.1:$CLUSTER_PORT"
data_dir       = "data"
bootstrap      = true

[cluster.mtls]
ca_cert    = "certs/ca.pem"
node_cert  = "certs/node.pem"
node_key   = "certs/node-key.pem"
enrol_addr = "127.0.0.1:$ENROL_PORT"

[gateway]
http_port = $GATEWAY_PORT

[memory]
data_dir = "data"

[memory.encryption]
key_ref = "env:LOBSLAW_MEMORY_KEY"

[memory.snapshot]
target = "local"

[trace]
enabled = true
EOF

say "booting"
LOBSLAW_MEMORY_KEY="$(head -c 32 /dev/urandom | base64)" \
  "$BIN" --config config.toml > node.log 2>&1 &
NODE_PID=$!

for _ in $(seq 1 60); do
  grep -q '"msg":"enrolment listener started"' node.log 2>/dev/null && break
  kill -0 "$NODE_PID" 2>/dev/null || fail "node exited during startup"
  sleep 1
done
grep -q '"msg":"enrolment listener started"' node.log \
  || fail "enrolment listener never started"

say "enrol request (laptop has no credential at all)"
OUT="$("$BIN" enrol request --addr "127.0.0.1:$ENROL_PORT" --ca-cert certs/ca.pem \
  --name alice --out laptop 2>&1)" || { echo "$OUT"; fail "enrol request"; }
ID="$(printf '%s' "$OUT" | sed -n 's/^Request submitted: //p')"
FP="$(printf '%s' "$OUT" | grep -o 'SHA256:[0-9a-f:]*' | head -1)"
[ -n "$ID" ] || fail "no request id"
[ -n "$FP" ] || fail "no fingerprint"
[ -f laptop/operator-key.pem ] || fail "the private key was not kept locally"
grep -q "PRIVATE KEY" laptop/operator-key.pem || fail "operator-key.pem is not a key"

say "bootstrap operator (the escape hatch)"
"$BIN" cluster export-operator bootstrap \
  --ca-cert certs/ca.pem --ca-key certs/ca-key.pem --out boot >/dev/null

OPFLAGS=(--addr "127.0.0.1:$CLUSTER_PORT" --ca-cert certs/ca.pem
         --node-cert boot/operator.pem --node-key boot/operator-key.pem)

say "the pending request is listed, with its fingerprint"
"$BIN" enrol list "${OPFLAGS[@]}" | grep -q "$FP" \
  || fail "the listing does not show the fingerprint the laptop printed"

say "a WRONG fingerprint is refused"
if "$BIN" enrol approve "$ID" --fingerprint SHA256:00:00 "${OPFLAGS[@]}" >/dev/null 2>&1; then
  fail "an approval with a mismatched fingerprint succeeded"
fi

say "the right fingerprint issues"
"$BIN" enrol approve "$ID" --fingerprint "$FP" "${OPFLAGS[@]}" | grep -q 'issued to "alice"' \
  || fail "approval did not issue"

say "the laptop collects it"
"$BIN" enrol status --addr "127.0.0.1:$ENROL_PORT" --ca-cert certs/ca.pem \
  --id "$ID" --out laptop >/dev/null
for f in operator.pem operator-ca.pem ca.pem; do
  [ -s "laptop/$f" ] || fail "laptop/$f was not written"
done

cat > "$XDG_CONFIG_HOME/lobslaw/contexts.toml" <<EOF
default = "smoke"

[contexts.smoke]
addr    = "127.0.0.1:$CLUSTER_PORT"
ca_cert = "$WORK/laptop/ca.pem"
cert    = "$WORK/laptop/operator.pem"
key     = "$WORK/laptop/operator-key.pem"
EOF

say "THE POINT: the enrolled credential reaches the cluster"
# This is what was broken while every unit test passed. The client
# verified its own certificate against the cluster CA, and the server
# had snapshotted a CA pool that predated the operator root.
"$BIN" memory list --context smoke | grep -q "127.0.0.1:$CLUSTER_PORT" \
  || fail "memory list did not reach the node, or did not name its source"
"$BIN" session list --context smoke | grep -q "127.0.0.1:$CLUSTER_PORT" \
  || fail "session list did not name its source"
# trace names the NODE, not just the address.
"$BIN" trace list --context smoke | grep -q "$NODE_ID" \
  || fail "trace did not name the node it read"

say "a second enrolment cannot reuse the first answer"
if "$BIN" enrol approve "$ID" --fingerprint "$FP" "${OPFLAGS[@]}" >/dev/null 2>&1; then
  fail "an already-decided request was approved twice"
fi

printf '\nSMOKE PASSED\n'
