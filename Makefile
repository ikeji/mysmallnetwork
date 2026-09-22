# Static builds so the binaries run on any Linux regardless of glibc version.
# VERSION comes from the git tag (or "dev"); the release workflow passes the tag.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS := -trimpath -ldflags="-s -w -X github.com/ikeji/mysmallnetwork/internal/buildinfo.version=$(VERSION)"
export CGO_ENABLED := 0

NATSIM_MODES := cone fullcone symmetric cone:fullcone fullcone:cone cone:symmetric symmetric:cone fullcone:symmetric symmetric:fullcone

.PHONY: all test unit natsim roam clean cross FORCE

all: bin/msnw

# Real file target so "sudo make natsim" reuses a binary built as the user.
# bin/.version changes only when VERSION does, so a new tag triggers a rebuild.
bin/msnw: go.mod go.sum $(shell find cmd internal -name '*.go') bin/.version
	go build $(GOFLAGS) -o bin/ ./cmd/...

bin/.version: FORCE
	@mkdir -p bin; echo "$(VERSION)" | cmp -s - $@ 2>/dev/null || echo "$(VERSION)" > $@

FORCE:

test: unit natsim roam

unit:
	go vet ./... && go test ./...

# NAT traversal matrix in network namespaces (Linux, needs nft + unshare).
natsim: bin/msnw
	@if [ "$$(uname -s)" != Linux ] || ! command -v unshare >/dev/null || \
	   ! { command -v nft >/dev/null || [ -x /usr/sbin/nft ]; }; then \
		echo "natsim: skipped (needs Linux, unshare and nft)"; exit 0; fi; \
	rc=0; for m in $(NATSIM_MODES); do \
		out=$$(test/natsim.sh $$m 2>&1) || rc=1; \
		echo "$$out" | grep -v 'reach the server\|server='; \
	done; exit $$rc

# Roaming: the client changes networks mid-session and must recover quickly.
roam: bin/msnw
	@if [ "$$(uname -s)" != Linux ] || ! command -v unshare >/dev/null || \
	   ! { command -v nft >/dev/null || [ -x /usr/sbin/nft ]; }; then \
		echo "roam: skipped (needs Linux, unshare and nft)"; exit 0; fi; \
	out=$$(test/natsim.sh cone -- test/roam.sh 2>&1); rc=$$?; \
	echo "$$out" | grep -v 'reach the server\|server='; exit $$rc

# Cross-compile for common targets into bin/<os>-<arch>/
cross:
	for t in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do \
		GOOS=$${t%/*} GOARCH=$${t#*/} go build $(GOFLAGS) -o bin/$${t%/*}-$${t#*/}/ ./cmd/... || exit 1; \
	done

clean:
	rm -rf bin
