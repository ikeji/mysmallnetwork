#!/bin/bash
# roam.sh — run inside natsim: move the client site to a new LAN address and
# a new NAT WAN address mid-session. A UDP flow must recover quickly and a
# TCP connection must survive with its byte stream intact.
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

"$ROOT/bin/msnw" server -server-key natsim -listen 10.0.0.1:4433 -relay 10.0.0.1:4434 >"$L/server.log" 2>&1 &
nsbg siteA python3 -c '
import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",21300))
while True:
    d,a=s.recvfrom(65535); s.sendto(d,a)
' >/dev/null 2>&1 &
sleep 0.3
nsbg siteA "$ROOT/bin/msnw" export -n sitea -u 21300 -t 21301 >"$L/exporter.log" 2>&1 &
sleep 1
nsbg siteA python3 -c '
import socket, threading
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("0.0.0.0",21301)); s.listen(5)
def h(c):
    while True:
        d=c.recv(65536)
        if not d: break
        c.sendall(d)
    c.close()
while True:
    c,_=s.accept(); threading.Thread(target=h,args=(c,),daemon=True).start()
' >/dev/null 2>&1 &
sleep 0.3
nsbg siteB "$ROOT/bin/msnw" client -v -n sitea -l udp:31300 >"$L/client.log" 2>&1 &
nsbg siteB "$ROOT/bin/msnw" client -v -n sitea:21301 -l 31301 >"$L/client-tcp.log" 2>&1 &
sleep 1.5

# TCP: one connection sends numbered lines for the whole test and checks the
# echoed sequence is complete and in order across the network change.
nsbg siteB python3 - "$LIMIT" <<'PY' &
import socket, sys, time
limit=float(sys.argv[1])
c=socket.create_connection(("127.0.0.1",31301)); c.settimeout(limit+20)
t0=time.time(); n=0; expect=0; buf=b""; stalled=None; longest=0.0
deadline=t0+10+limit
while time.time()<deadline or expect<n:
    if time.time()<deadline:
        c.sendall(f"{n}\n".encode()); n+=1
    try:
        d=c.recv(65536)
    except socket.timeout:
        print("tcp: FAILED, echo stalled for good"); sys.exit(1)
    if not d:
        print("tcp: FAILED, connection closed"); sys.exit(1)
    buf+=d
    while b"\n" in buf:
        line,buf=buf.split(b"\n",1)
        if int(line)!=expect:
            print(f"tcp: FAILED, got {line!r} want {expect}"); sys.exit(1)
        expect+=1
    time.sleep(0.2)
c.close()
print(f"tcp: OK, {expect} lines echoed in order through the network change")
PY
TCP=$!

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
wait $TCP || rc=1
kill $(jobs -p) 2>/dev/null
[ $rc -ne 0 ] && echo "roam: logs in $L" || rm -rf "$L"
exit $rc
