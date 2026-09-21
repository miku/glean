BINARY  := glean
GO      := go
GOFLAGS := -trimpath -ldflags='-s -w'

.PHONY: all build test fmt vet clean snapshot release

all: build

build: $(BINARY)

$(BINARY): $(wildcard *.go)
	$(GO) build $(GOFLAGS) -o $@ .

test:
	$(GO) test ./...

fmt:
	gofmt -w *.go

vet:
	$(GO) vet ./...

clean:
	rm -f $(BINARY)
	rm -rf dist

# Build archives and deb/rpm packages into ./dist without publishing. Use this
# to sanity-check a release before tagging.
snapshot:
	goreleaser release --snapshot --clean

# Publish a release. Requires a git tag (e.g. vX.Y.Z) and GITHUB_TOKEN; the
# version is taken from the tag, see .goreleaser.yaml.
release:
	goreleaser release --clean --skip=archive
