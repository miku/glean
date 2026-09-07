BINARY  := expiringsoon
GO      := go
GOFLAGS := -trimpath -ldflags='-s -w'

.PHONY: all build test fmt vet clean

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
