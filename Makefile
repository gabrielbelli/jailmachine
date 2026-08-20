GO ?= go

.PHONY: build test lint e2e clean

build:
	$(GO) build -o jm ./cmd/jm

test:
	$(GO) vet ./...
	$(GO) test ./...

lint:
	@out=$$(gofmt -l . ); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

e2e:
	$(GO) test -tags e2e ./e2e/...

clean:
	rm -rf jm dist
