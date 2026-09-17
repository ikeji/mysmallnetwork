#!/bin/bash
# roam.sh — run inside natsim: move the client site to a new LAN address and
# a new NAT WAN address mid-session and require a UDP flow to recover fast.
#
#   test/natsim.sh cone -- test/roam.sh
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
L=${NATSIM_LOG:-$(mktemp -d)}
LIMIT=${ROAM_LIMIT:-15}   # seconds allowed for recovery
export MSNW_KEY=roam MSNW_SERVER_KEY=natsim MSNW_SERVER=10.0.0.1:4433 QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING=true
ns() { ip netns exec "$@"; }
# For background jobs: exec replaces the forked subshell so "kill $(jobs -p)"
# reaches the command itself instead of an intermediate shell.
nsbg() { exec ip netns exec "$@"; }

"$ROOT/bin/msnw-server" -server-key natsim -listen 10.0.0.1:4433 -relay 10.0.0.1:4434 >"$L/server.log" 2>&1 &
nsbg siteA python3 -c '
import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",21300))
while True:
    d,a=s.recvfrom(65535); s.sendto(d,a)
' >/dev/null 2>&1 &
sleep 0.3
nsbg siteA "$ROOT/bin/msnw-exporter" -n sitea -t 21300 >"$L/exporter.log" 2>&1 &
sleep 1
nsbg siteB "$ROOT/bin/msnw-client" -v -n sitea -l udp:31300 >"$L/client.log" 2>&1 &
sleep 1.5

nsbg siteB python3 - "$LIMIT" <<'PY' &
import socket, sys, time
limit=float(sys.argv[1])
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(0.4)
t0=time.time(); outage=None; recovered=None
while time.time()-t0 < 10+limit:
    s.sendto(b"x",("127.0.0.1",31300))
    try:
        s.recvfrom(100); now=time.time()-t0
        if outage is not None and recovered is None:
            recovered=now-outage
    except socket.timeout:
        now=time.time()-t0
        if outage is None: outage=now
    time.sleep(0.5)
if outage is None:
    print("roam: no outage observed (address change had no effect?)"); sys.exit(1)
if recovered is None:
    print(f"roam: FAILED, no recovery within {limit}s"); sys.exit(1)
print(f"roam: OK, recovered {recovered:.1f}s after the network change")
sys.exit(0 if recovered <= limit else 1)
PY
PING=$!
sleep 5
ns siteB ip addr add 192.168.2.11/24 dev ethB
ns siteB ip addr del 192.168.2.10/24 dev ethB
ns siteB ip route replace default via 192.168.2.1
ns natB ip addr add 10.0.0.4/24 dev wanB
ns natB ip addr del 10.0.0.3/24 dev wanB
wait $PING; rc=$?
kill $(jobs -p) 2>/dev/null
[ $rc -ne 0 ] && echo "roam: logs in $L" || rm -rf "$L"
exit $rc
