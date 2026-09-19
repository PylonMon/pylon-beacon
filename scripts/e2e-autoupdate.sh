#!/usr/bin/env bash
# End-to-end proof of auto-update on this OS: an agent built as 0.9.90 finds a
# "release" 0.9.91 on a local server, verifies it, makes it check in, swaps
# itself and carries on — and the check-ins never stop. Run from the repo root.
set -euo pipefail
R=$(mktemp -d); trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$R"' EXIT
mkdir -p "$R/rel" "$R/agent"
OS=$(go env GOOS); ARCH=$(go env GOARCH)
LD="-X main.releaseBase=http://127.0.0.1:18461/ -X main.updateFirstCheck=2s"
go build -ldflags "$LD -X main.version=0.9.91" -o "$R/rel/pylon-beacon-$OS-$ARCH" .
go build -ldflags "$LD -X main.version=0.9.90" -o "$R/agent/pylon-beacon" .
( cd "$R/rel" && echo 0.9.91 > VERSION && sha256sum pylon-beacon-* > SHA256SUMS && python3 -m http.server 18461 --bind 127.0.0.1 >/dev/null 2>&1 ) &
# a stand-in for PylonMon's ingest: records each check-in's agent version
python3 - "$R/checkins.log" <<'PY' &
import sys, http.server
log = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', 0)))
        open(log, 'a').write(self.headers.get('User-Agent', '?') + '\n')
        self.send_response(200); self.send_header('Content-Type', 'application/json'); self.end_headers()
        self.wfile.write(b'{"ok":true}')
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', 18460), H).serve_forever()
PY
sleep 2
printf 'key = k\nurl = http://127.0.0.1:18460\nnode = e2e\ninterval = 5\nauto_update = true\n' > "$R/agent/beacon.conf"
"$R/agent/pylon-beacon" -config "$R/agent/beacon.conf" > "$R/agent.log" 2>&1 &
PID=$!
sleep 40
echo "--- agent log ---"; cat "$R/agent.log"
echo "--- check-ins by agent version ---"; sort "$R/checkins.log" | uniq -c
grep -q "auto-update: 0.9.90 -> 0.9.91" "$R/agent.log" || { echo "FAIL: the agent never switched"; exit 1; }
grep -q "pylon-beacon/0.9.91" "$R/checkins.log" || { echo "FAIL: no check-in from the new version"; exit 1; }
test -f "$R/agent/pylon-beacon.old-0.9.90" || { echo "FAIL: the previous binary was not kept"; exit 1; }
"$R/agent/pylon-beacon" -version | grep -q "0.9.91" || { echo "FAIL: the binary in place is not the new one"; exit 1; }
if [ "$OS" != "windows" ]; then
  kill -0 "$PID" 2>/dev/null || { echo "FAIL: the agent process died (it should have replaced itself in place)"; exit 1; }
  [ "$(tr '\0' ' ' < /proc/$PID/cmdline 2>/dev/null | wc -c)" -gt 0 ] && echo "same PID $PID still running — replaced in place"
fi
N=$(wc -l < "$R/checkins.log"); [ "$N" -ge 7 ] || { echo "FAIL: only $N check-ins in 40s at a 5s interval — there was a gap"; exit 1; }
echo "PASS: $N check-ins, switched 0.9.90 -> 0.9.91 with none missed"
