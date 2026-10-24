# mysmallnetwork (msnw)

## Tests

- `make test` = `unit` (go vet + go test) + `natsim` (NAT traversal matrix) + `roam` (network change mid-session).
- `natsim` / `roam` run **without sudo** on Linux: `test/natsim.sh` re-execs itself in an
  unprivileged user namespace (`unshare -U --keep-caps -n -m`). Just run `make natsim roam`.
  Needs `unshare` and `nft`. On macOS / without them they are skipped.
- CI (`.github/workflows`) wraps them in `sudo` only because GitHub runners block
  unprivileged user namespaces. That is not a hint that sudo is needed locally.
- A failing run leaves its logs in a temp dir and prints `roam(MODE): logs in /tmp/...`.
  `ROAM_KEEP_LOG=1` keeps them on success too; `ROAM_MODE=switch|handover|exporter`,
  `ROAM_LIMIT=<sec>` tune a single run: `ROAM_MODE=handover test/natsim.sh cone -- test/roam.sh`.

## When changing the CLI

`test/natsim.sh` and `test/roam.sh` invoke `bin/msnw` (`server`, `export`, `import`, ...)
with literal arguments. Any change to a subcommand's flags or argument syntax must update
those scripts too, otherwise the processes exit on a usage error and the tests fail with
unrelated-looking symptoms (e.g. `ConnectionRefusedError` from the Python probes because the
importer never bound its port). Also grep `README.md`, `README.ja.md`, `examples/` and
`android/` for the old spelling.

Current `msnw import` syntax (since v0.1.13, `-l` is gone):

    msnw import [[[LADDR]:]LPORT=]NAME[:PORT|@HOST:PORT][/udp] ...
    31300=sitea/udp        # local udp 31300 -> exporter's default UDP port (-u)
    31301=sitea:21301      # local tcp 31301 -> port 21301 on the exporter
    8080=NAME@host:80      # any host, through an --all exporter

## Build

- `make` builds static `bin/msnw` (CGO disabled); `make cross` for all targets.
- VERSION comes from `git describe`; the release workflow passes the tag.
