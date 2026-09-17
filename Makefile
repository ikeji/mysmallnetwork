# Static builds so the binaries run on any Linux regardless of glibc version.
GOFLAGS := -trimpath -ldflags="-s -w"
export CGO_ENABLED := 0

.PHONY: all test clean cross

all:
	go build $(GOFLAGS) -o bin/ ./cmd/...

test:
	go vet ./... && go test ./...

# Cross-compile for common targets into bin/<os>-<arch>/
cross:
	for t in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do \
		GOOS=$${t%/*} GOARCH=$${t#*/} go build $(GOFLAGS) -o bin/$${t%/*}-$${t#*/}/ ./cmd/... || exit 1; \
	done

clean:
	rm -rf bin
