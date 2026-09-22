# msnw — my small network

[日本語](README.ja.md)

A small tunnel: publish a service under a name, and reach it peer-to-peer
from behind NAT.

```
        ┌──────────────┐  introduction / NAT traversal help / relay of last resort
        │ msnw server  │  (QUIC control :4433, UDP relay :4434)
        └──────┬───────┘
   register ↗          ↖ lookup
┌──────────────┐  QUIC (P2P, direct or relayed)  ┌──────────────┐
│ msnw export  │ ◀═════════════════════════════▶ │ msnw client  │
│  -n hogehoge │   1 TCP connection = 1 stream   │              │
│  -t 1234     │                                 │ stdio / -l / │
└──────┬───────┘                                 │   --socks5   │
       ▼                                         └──────────────┘
   nc -l 1234
```

## Install

Download the archive for your OS / CPU from
[Releases](https://github.com/ikeji/mysmallnetwork/releases) and unpack it: it
is a single static binary. On Linux x86_64:

```
curl -L https://github.com/ikeji/mysmallnetwork/releases/latest/download/msnw-linux-amd64.tar.gz | tar xz
./msnw -h
```

With Go installed, `go install github.com/ikeji/mysmallnetwork/cmd/msnw@latest` works too.

## Build

```
make            # static binary bin/msnw (CGO_ENABLED=0) with the server / export / client / mosh / gen-key subcommands
make test       # unit tests + the full NAT simulation matrix (make unit / make natsim / make roam individually)
make cross      # linux/darwin/windows builds into bin/<os>-<arch>/
```

Needs Go 1.26 or newer (a quic-go requirement). If you run `go build` yourself,
set `CGO_ENABLED=0`: a cgo build links libc dynamically and fails on hosts
with an older glibc (`GLIBC_2.34' not found`).

## Quick guide: share your ssh / mosh

You want to ssh or mosh from a laptop on the road into a PC at home that runs
sshd. Both can sit behind NAT, and you do not need to run a server: the public
server relay.ikeji.ma is the default.

**1. Put the binary on both machines**

Download the archive for each machine's OS / CPU from
[Releases](https://github.com/ikeji/mysmallnetwork/releases) and unpack it
(see [Install](#install)). The single `msnw` file is all you need; it does not
have to be on PATH. Building from source with `make` works too.

**2. Pick a link key**

Use the same string on both machines; below it is `mylonglongsecretkey`. Only
people who know it can connect, so make it long and hard to guess
(`msnw gen-key` prints a random one).

**3. Publish on the home PC (the sshd side)**

```
msnw export -key mylonglongsecretkey -n home -t 22 -u 60001-60999
```

`-t 22` is sshd, `-u 60001-60999` is the UDP port range for mosh (drop `-u` if
you only need ssh). A range lets you open any number of mosh sessions at once
(each uses one port). `home` is any name you like; a different link key means a
different namespace, so it never collides with someone else's `home`. Once the
log says `registered "home"` it is ready. Leave it running.

**4a. mosh from the laptop**

```
msnw mosh -key mylonglongsecretkey user@home
```

That is all. Internally it starts `mosh-server` over ssh (with msnw itself as
the ProxyCommand), forwards mosh's UDP through the tunnel and runs
`mosh-client`. The laptop needs `ssh` and `mosh-client`, the home PC needs
`mosh-server`. To use another port range pass `-p 60001:60010` and match the
exporter's `-u`.

**4b. ssh from the laptop**

```
ssh -o ProxyCommand='msnw client -key mylonglongsecretkey -n home' user@home
```

The host name `home` is only what ssh displays; the ProxyCommand makes the
actual path. Put it in `~/.ssh/config` and `ssh home` is enough (`msnw mosh`
uses the same entry):

```
Host home
    User user
    ProxyCommand /path/to/msnw client -key mylonglongsecretkey -n home
```

The key can also come from the `MSNW_KEY` environment variable, so if you do
not want it on a command line, `export MSNW_KEY=mylonglongsecretkey` and drop
`-key`.

**Alternative: a local port**

Without a ProxyCommand, keep the laptop's port 2222 connected to port 22 at
home:

```
msnw client -key mylonglongsecretkey -n home -l 2222   # leave it running
ssh -p 2222 user@localhost                             # scp and rsync work the same way
```

**What to look for**

- On the first connection the client logs `via direct ...` or `via relay ...`.
  `direct` means the NAT traversal worked; `relay` means the server is
  forwarding packets (encryption is the same either way).
- When the network changes (switching Wi-Fi, for example), both ssh and mosh
  pause for a few seconds and then carry on where they were (the log says
  `session ... resumed`).

## Usage in detail

There are two keys, both shared secrets:

- **Link key** `-key` (`$MSNW_KEY`): shared between exporter and client; they
  only connect if it matches. The server never sees it. `msnw gen-key` makes one.
- **Server key** `-server-key` (`$MSNW_SERVER_KEY`): an admission ticket that
  keeps strangers off your server. If the server is started without one,
  anyone may use it.

Exporter and client default to the public server `relay.ikeji.ma:4433` (no
server key), so a downloaded binary plus a link key is all you need. To use
your own server, point at it with `-s host:port` (or `$MSNW_SERVER`).

### server

```
msnw server [-server-key S] [-listen :4433] [-relay :4434] [-key server.key]
```

Both UDP ports must be reachable from outside. With `-key` the private key is
saved so the fingerprint stays the same across restarts; the
`fingerprint: sha256:...` printed at startup can be pinned by exporter and
client with `-server-fp` (or `$MSNW_SERVER_FP`).

### export

```
msnw export -key LINKKEY -n hogehoge -t 1234       # publish localhost:1234 (TCP) as hogehoge
msnw export -n hogehoge -t 1234 -t 8080 -t db:5432 # several targets; the first is the default
msnw export -n home -t 22 -u 60001-60999           # -t is TCP, -u is UDP; ranges allowed (mosh)
msnw export -n exit --all                          # forward to any host:port (exit-node style)
```

`-t` (TCP) and `-u` (UDP) take `port` (meaning localhost:port), `host:port`, or
a range `lo-hi` / `host:lo-hi`. `--all` allows any destination on both
protocols; combined with `-t`, the first `-t` is the default target.

### client

```
msnw client -key LINKKEY -n hogehoge    # pipe stdin/stdout (nc style, ssh ProxyCommand)
                                        # below, -key is assumed to be in $MSNW_KEY
msnw client -n hogehoge:8080            # another port on the exporter
msnw client -n exit:example.com:80      # any host through an --all exporter

msnw client -n hogehoge -l              # listen on 127.0.0.1 on the exporter's default port number
msnw client -n hogehoge -l 5000         # 127.0.0.1:5000 -> hogehoge's default target
msnw client -n hogehoge:8080 -l :5000   # listen on all interfaces
msnw client -n hogehoge:60001 -l udp:60001   # forward UDP (one flow per source address)

msnw client --socks5                    # SOCKS5 on 127.0.0.1:1080; msnw names go through the tunnel, the rest directly
msnw client --socks5 :1080 -n exit      # unknown hosts go out through exit
```

How SOCKS5 destinations are interpreted:

| destination host        | goes to                                      |
|-------------------------|----------------------------------------------|
| `hogehoge`              | exporter hogehoge, the requested port (*)    |
| `hogehoge.msnw`         | same                                         |
| `db.hogehoge.msnw`      | `db:<port>` as seen from exporter hogehoge   |
| anything else (FQDN/IP) | the default exporter given with `-n`; without `-n`, a direct connection from this machine |

(*) If the exporter publishes exactly one TCP target (`msnw export -n web -t 8765`),
the name denotes a service and the port is ignored, so `http://web/` works.
With several `-t` or `--all` the name denotes a host and the port selects the
target. This only applies to SOCKS5; an explicit `-n web:80` stays strict.

Because everything else is reached directly, the proxy can stay configured in
a browser all the time. It listens on 127.0.0.1 by default; binding another
address (`--socks5 :1080`) lets other machines use it, direct connections
included.

For the `.msnw` forms let the proxy resolve names, e.g. `curl --socks5-hostname`
or `ssh -o ProxyCommand='nc -X 5 -x ... %h %p'`.

Example: a web service in the browser

Publish it on the machine that runs it, and start the proxy on the machine
with the browser:

```
msnw export -key K -n mypc -t 8765      # the service runs on this machine
msnw client -key K --socks5             # on the machine with the browser
```

Then point the browser at `http://mypc/` (or `http://mypc.msnw/`). Because
`mypc` publishes a single target, the port is not needed. Every other site is
reached directly, so the proxy can stay on all the time.

- **Firefox**: Settings → Network Settings → Manual proxy configuration, SOCKS
  Host `127.0.0.1`, Port `1080`, SOCKS v5, and tick "Proxy DNS when using SOCKS
  v5". Without that last option Firefox tries to resolve `mypc` itself and
  fails.
- **Chrome / Chromium / Edge**: SOCKS5 sends names through the proxy by
  default, but the setting is not in the UI; start the browser with
  `--proxy-server="socks5://127.0.0.1:1080"`, or use an extension such as
  FoxyProxy / SwitchyOmega and add a rule for `*.msnw` and your exporter names.
- **Safari / macOS system proxy**: System Settings → Network → Details →
  Proxies → SOCKS proxy `127.0.0.1:1080`. This applies to every app that uses
  the system proxy.
- **curl**: `curl --socks5-hostname 127.0.0.1:1080 http://mypc/` (with plain
  `--socks5` curl resolves the name locally and fails).

Example: ssh

```
ssh -o ProxyCommand='msnw client -n hogehoge:22' user@anything
```

Example: mosh (`msnw mosh`)

```
msnw export -key K -n home -t 22 -u 60001-60999   # sshd as the default target, plus mosh's UDP range
msnw mosh -key K user@home                      # -p changes the port range (default 60001:60999)
```

`msnw mosh` starts `mosh-server` over ssh (passing its own executable as the
ProxyCommand) bound to 127.0.0.1 only, forwards a local UDP port to the same
port on the exporter side inside the same process, then runs `mosh-client`
against 127.0.0.1. Extra ssh options go in `-ssh "..."` or `MSNW_MOSH_SSH`. The
link key reaches the child process through the environment, not the command
line. Flags may follow the host (`msnw mosh user@home -v`). While mosh owns
the terminal, log output is invisible, so use `-log FILE` (or `MSNW_LOG`) to
append it to a file; both the in-process forwarder and the ssh ProxyCommand
child log there.

## How it works

- **Resumable TCP sessions**: one TCP connection is one QUIC stream, but a thin
  layer on top of the stream keeps a replay buffer and ACKs, so a TCP
  connection survives the tunnel being re-established (see "Roaming").
- **UDP**: datagrams received on `-l udp:PORT` travel as QUIC datagrams
  (RFC 9221). Anything too large for one packet goes length-framed over the
  flow's own stream. There is one flow per local source address; it closes
  after 10 minutes of silence.
- **One socket**: each node uses a single UDP socket both for the control
  connection to the server and for peer connections, so the public address the
  server observes on the control connection is exactly the NAT mapping the
  peers punch through (STUN-like).
- **Introduction**: when the client looks up the name given with `-n`, the
  server hands the exporter the client's candidate addresses (reflexive plus
  LAN) and the client the exporter's.
- **Hole punching**: both sides send small UDP packets to all of the other's
  candidates while the client dials QUIC to all of them in parallel; the first
  handshake to finish wins. The client also treats the source address of any
  punch it receives as a candidate, so an exporter behind a symmetric NAT is
  still reachable when the client side is a full cone.
- **Relay**: if no direct connection is up after 1.5 seconds, QUIC is set up
  through the server's relay port. The relay forwards UDP verbatim, so
  encryption stays end-to-end. Symmetric-to-symmetric NATs end up here.
- **Namespaces**: the exporter registers `HMAC(link key, name)` rather than the
  name, and the client looks up the same value. The server learns neither the
  name nor the key, and different keys never collide even for the same name.
- **Authentication**: every proof of a shared key is an HMAC over that TLS
  session's exported keying material, so it cannot be replayed or forwarded to
  another connection. The server key is proven to the server; the link key is
  proven in both directions on the first stream of every peer connection. The
  exporter accepts no CONNECT and the client sends no data before that. The
  public-key fingerprints exchanged through the server are still pinned, which
  rejects TLS from anyone the server did not introduce.

## Roaming (when the network changes)

The client checks its own address list every 2 seconds; on a change it drops
its peer and server connections immediately and redials. If the path dies
without an address change (a NAT mapping expired, say), the QUIC idle timeout
catches it (30 seconds, keepalive every 10).

TCP connections survive the redial thanks to the resumable session layer
(`internal/resume`):

- CONNECT issues a token. Both ends count the payload bytes they sent and
  received and keep whatever the peer has not acknowledged in a replay buffer
  (ACKs every 64 KiB or every second).
- When the tunnel breaks, neither end closes its local socket. The client sends
  `RESUME <token> <received>` on a fresh connection, the exporter answers with
  its own count, and each side retransmits only what the other missed.
- A session that waits more than 5 minutes for a resume is closed. To ssh this
  looks like a pause of a few seconds. A short `ServerAliveInterval` in ssh can
  make ssh itself give up during that pause.

stdio (ProxyCommand), `-l` and SOCKS5 all use the same layer. UDP flows are
recreated on the next packet, which is how mosh comes back within seconds.

`make roam` (`test/natsim.sh cone -- test/roam.sh`) changes the client site's
LAN address and NAT WAN address mid-session and checks the UDP recovery time
and that a numbered TCP echo continues without loss or duplication. A real sshd
and ssh placed in siteA / siteB, with the network changed during a running
command, finish with all output and exit code 0.

## Security model

- The server is not trusted. A compromised (or impersonated) server can only
  disrupt connections and observe which hashes connected when and from where.
  Terminating TLS on both sides and forwarding does not work: the link-key
  proof is different for every session.
- Whoever holds the link key can be either client or exporter (the roles are
  symmetric), so within a group sharing a key, names can be spoofed. Use one key
  per trust boundary; "this person only gets this port" is expressed with a
  separate key and exporter.
- The server sees the name hashes, so a guessable name with a short key can be
  brute-forced. Use keys from `msnw gen-key`.
- The server is neither an amplifier nor a reflector. The control port
  validates the source address with a Retry before the handshake (a spoofed
  source gets back fewer bytes than it sent). The relay never answers hellos,
  and accepts a session's hello only from the IP seen on that party's control
  connection, so a spoofed address cannot be bound as a relay endpoint.
  Forwarding is one to one.
- The server never opens outbound connections. Only an `--all` exporter can
  reach arbitrary destinations, and only for holders of its link key.
- Revocation means handing out a new key.
- One link key per process. If SOCKS5 ever needs to span exporters with
  different keys, this extends to a prioritized list (the client already looks
  keys up per name).

## NAT traversal tests (test/natsim.sh)

Builds "a public server plus two sites behind NAT" out of Linux network
namespaces and exercises real hole punching and the relay fallback. No sudo
needed: it runs in a user namespace (mapped to your own uid, not root, keeping
only the capabilities via `unshare --keep-caps`). Needs `nft` (package
`nftables`) and `iproute2`.

```
make
test/natsim.sh cone        # a typical router; expects a direct connection
test/natsim.sh symmetric   # ports change per destination; expects the relay
test/natsim.sh fullcone    # accepts from any source (like a UPnP port mapping)
test/natsim.sh cone:symmetric   # one type per side (siteA:siteB)
test/natsim.sh cone -- bash   # just build the topology and open a shell (ip netns exec siteA ...)
NATSIM_OPEN_INPUT=1 test/natsim.sh cone   # a NAT that does not drop unsolicited WAN input (see below)
```

Topology: `siteA 192.168.1.10 ─ natA(10.0.0.2) ─ br0 ─ natB(10.0.0.3) ─ siteB 192.168.2.10`,
server at 10.0.0.1. The exporter runs in siteA, the client in siteB.

Findings:

- Two ordinary routers (cone masquerade that drops unsolicited packets in the
  WAN INPUT chain) connect directly.
- Symmetric (`masquerade fully-random`) cannot connect directly and uses the
  relay, and so does any cone/symmetric mix in either direction: Linux
  masquerade filters by address and port (port-restricted cone), so the cone
  side rejects the symmetric side's new port.
- A full cone on either side makes a direct connection possible even against a
  symmetric peer. With a symmetric exporter and a full-cone client, the port the
  server saw for the exporter is useless, but the exporter's punch reaches the
  client, which learns the source address and dials it. Masquerade cannot make
  a full cone, so the simulator uses a static DNAT of the node's port instead
  (siteA pins UDP 40001 and siteB 40002 with `-port`).

| siteA (exporter) : siteB (client) | result |
|---|---|
| cone : cone | direct |
| fullcone : cone / cone : fullcone / fullcone : fullcone | direct |
| fullcone : symmetric | direct |
| symmetric : fullcone | direct (candidate learned from the punch) |
| cone : symmetric / symmetric : cone / symmetric : symmetric | relay |

- Between two NATs that do not drop unsolicited WAN input, a punch that arrives
  first is recorded by conntrack as an inbound flow, after which the NAT will
  not reuse the same port for its own outbound flow (the mapping stops being
  endpoint-independent). With both sides punching at once this cannot be
  avoided, so those fall back to the relay.

## Notes

- The server certificate is not verified by default. Landing on a fake server
  leaks nothing (the peers just fail to connect), but to rule out disruption,
  pin the server with `-key` on the server side and `-server-fp` on the nodes.
- On Linux, quic-go warns when the UDP receive buffer is small. For high
  throughput: `sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000`.
- For debugging, `MSNW_FORCE_RELAY=1` makes the client skip direct paths and
  use only the relay; `-v` shows why each dial failed.
