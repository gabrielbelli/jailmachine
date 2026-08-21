GO ?= go
MODULE := github.com/gabrielbelli/jailmachine
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION) \
           -X $(MODULE)/internal/version.Commit=$(COMMIT) \
           -X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: install build test lint e2e release-snapshot image clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o jm ./cmd/jm

test:
	$(GO) vet ./...
	$(GO) test ./...

# lint: gofmt only for now (no golangci-lint dependency).
lint:
	@out=$$(gofmt -l . ); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

e2e:
	$(GO) test -tags e2e ./e2e/...

# release-snapshot: what the release workflow does, minus publishing.
# Output lands in dist/ (git-ignored).
release-snapshot:
	goreleaser check
	goreleaser release --snapshot --clean --skip=publish

# image: build the prebaked guest image locally (Mac with HVF, ~10 min),
# then publish it with:
#   gh release create guest-<ver> dist/*.zst dist/*.sha256 --title "guest <ver>"
RELEASE ?= 15.1-RELEASE
image: build
	./jm image build --release $(RELEASE) --out dist

clean:
	rm -rf jm dist

PREFIX ?= /opt/homebrew
install: ## build and install jm into $(PREFIX)/bin
	go build -ldflags "$(LDFLAGS)" -o $(PREFIX)/bin/jm ./cmd/jm
	ln -sf jm $(PREFIX)/bin/jpodman
