#!/bin/bash
# natsim.sh — simulate two NATed sites and a public server with Linux netns.
#
#   usage: test/natsim.sh [cone|symmetric] [-- cmd...]
#
# Runs entirely inside an unprivileged user namespace (no sudo). Needs
# iproute2 and nft (package "nftables"; set NFT=/path/to/nft to override).
#
#        siteA 192.168.1.10 ── natA ── 10.0.0.2 ─┐
#                                                 br0 (10.0.0.0/24) ── 10.0.0.1 server
#        siteB 192.168.2.10 ── natB ── 10.0.0.3 ─┘
#
# cone:      masquerade (endpoint-independent mapping; hole punching works)
# symmetric: masquerade fully-random (per-destination ports; relay needed)
#
# Without a command it runs the built-in smoke test: exporter in siteA,
# client in siteB, once direct-preferred and once with MSNW_FORCE_RELAY.
set -eu
MODE=${1:-cone}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
NFT=${NFT:-nft}

if [ -z "${NATSIM_INNER:-}" ]; then
	command -v "$NFT" >/dev/null || { echo "nft not found; apt install nftables or set NFT=" >&2; exit 1; }
	[ -x "$ROOT/bin/msnw-server" ] || { echo "build first: make" >&2; exit 1; }
	exec env NATSIM_INNER=1 unshare -Urnm "$0" "$@"
fi
shift || true
[ "${1:-}" = "--" ] && shift

# We own a private mount namespace: give "ip netns" a writable /run.
mount -t tmpfs tmpfs /run

MASQ="masquerade"
[ "$MODE" = symmetric ] && MASQ="masquerade fully-random"

# Typical routers drop unsolicited WAN packets in INPUT, so no conntrack entry
# is confirmed for them. Without that rule an early punch from the peer is
# recorded as an inbound flow and the NAT then refuses to reuse the source
# port for our own outbound flow (mapping stops being endpoint-independent).
# NATSIM_OPEN_INPUT=1 reproduces that unfriendly behaviour.
INPUT_RULE_TMPL='iifname "wan%s" ct state new drop'
[ -n "${NATSIM_OPEN_INPUT:-}" ] && INPUT_RULE_TMPL=''

ns() { ip netns exec "$@"; }

ip link set lo up
ip link add br0 type bridge
ip addr add 10.0.0.1/24 dev br0
ip link set br0 up

mksite() { # name wan_ip lan_prefix
	local n=$1 wan=$2 lan=$3
	local INPUT_RULE; INPUT_RULE=$(printf "$INPUT_RULE_TMPL" "$n")
	ip netns add nat$n
	ip netns add site$n
	ip link add wan$n type veth peer name br$n
	ip link set br$n master br0 up
	ip link set wan$n netns nat$n
	ip link add lan$n type veth peer name eth$n
	ip link set lan$n netns nat$n
	ip link set eth$n netns site$n

	ns nat$n ip link set lo up
	ns nat$n ip addr add $wan/24 dev wan$n
	ns nat$n ip addr add $lan.1/24 dev lan$n
	ns nat$n ip link set wan$n up
	ns nat$n ip link set lan$n up
	ns nat$n sh -c "echo 1 > /proc/sys/net/ipv4/ip_forward"
	ns nat$n "$NFT" -f - <<-NFT
		table ip nat {
			chain post { type nat hook postrouting priority srcnat; oifname "wan$n" $MASQ; }
		}
		table ip filter {
			chain filter_forward { type filter hook forward priority 0; policy drop;
				iifname "lan$n" accept
				ct state established,related accept
			}
			chain filter_input { type filter hook input priority 0; policy accept;
				$INPUT_RULE
			}
		}
	NFT

	ns site$n ip link set lo up
	ns site$n ip addr add $lan.10/24 dev eth$n
	ns site$n ip link set eth$n up
	ns site$n ip route add default via $lan.1
}
mksite A 10.0.0.2 192.168.1
mksite B 10.0.0.3 192.168.2

echo "natsim: mode=$MODE  server=10.0.0.1  siteA=192.168.1.10 (behind 10.0.0.2)  siteB=192.168.2.10 (behind 10.0.0.3)"
ns siteA ping -c1 -W1 10.0.0.1 >/dev/null && ns siteB ping -c1 -W1 10.0.0.1 >/dev/null && echo "natsim: both sites reach the server"
ns siteB ping -c1 -W1 192.168.1.10 >/dev/null 2>&1 && { echo "natsim: BUG sites can reach each other directly"; exit 1; }

if [ $# -gt 0 ]; then
	exec "$@"
fi

# ---- built-in smoke test ----------------------------------------------------
export MSNW_SECRET=natsim MSNW_SERVER=10.0.0.1:4433 QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING=true
LOG=${NATSIM_LOG:-$(mktemp -d)}
"$ROOT/bin/msnw-server" -listen 10.0.0.1:4433 -relay 10.0.0.1:4434 >"$LOG/server.log" 2>&1 &
ns siteA python3 -c '
import socket,threading
s=socket.socket(); s.bind(("0.0.0.0",1234)); s.listen(5)
while True:
    c,_=s.accept(); c.sendall(b"echo:"+c.recv(100)); c.close()
' >"$LOG/echo.log" 2>&1 &
sleep 0.3
ns siteA "$ROOT/bin/msnw-exporter" -v -n sitea -t 1234 >"$LOG/exporter.log" 2>&1 &
sleep 1

rc=0
for force in "" 1; do
	label=$([ -n "$force" ] && echo "forced-relay" || echo "auto")
	out=$(echo "hi-$label" | MSNW_FORCE_RELAY=$force ns siteB timeout 30 "$ROOT/bin/msnw-client" -v -n sitea 2>"$LOG/client-$label.log" || true)
	via=$(grep -o 'via .*' "$LOG/client-$label.log" | head -1)
	if [ "$out" = "echo:hi-$label" ]; then
		echo "natsim: [$MODE/$label] OK  ($via)"
	else
		echo "natsim: [$MODE/$label] FAILED (got '$out'); logs in $LOG"; rc=1
	fi
done
[ "$MODE" = cone ] && [ -z "${NATSIM_OPEN_INPUT:-}" ] && ! grep -q 'via direct' "$LOG/client-auto.log" && { echo "natsim: expected a direct connection in cone mode"; rc=1; }
[ "$MODE" = symmetric ] && ! grep -q 'via relay' "$LOG/client-auto.log" && { echo "natsim: expected the relay in symmetric mode"; rc=1; }
kill $(jobs -p) 2>/dev/null
[ $rc -eq 0 ] && rm -rf "$LOG"
exit $rc
