GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test test-linux test-integration lint fmt vet cross info demo clean

all: lint test

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/minic ./cmd/minic

## test: the portable tests, which run on any OS
test:
	CGO_ENABLED=0 $(GO) test -count=1 ./...

## test-integration: the real container tests. Linux and root only.
test-integration:
	@[ "$$(uname)" = "Linux" ] || { echo "containers need Linux; see the README for how to run this in Docker"; exit 1; }
	sudo -E $(GO) test -tags integration -count=1 -v ./internal/container/...

## cross: prove both the Linux and the stub build paths still compile
cross:
	CGO_ENABLED=0 GOOS=linux  $(GO) build ./...
	CGO_ENABLED=0 GOOS=darwin $(GO) build ./...
	CGO_ENABLED=0 GOOS=linux  $(GO) vet -tags integration ./...

lint: fmt vet

fmt:
	@unformatted=$$(gofmt -l . | grep -v '^$$' || true); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

vet:
	CGO_ENABLED=0 $(GO) vet ./...

## info: report what this host supports
info: build
	./bin/minic info

## demo: run a container in Docker, since the host is probably not Linux
demo:
	docker run --rm -it --privileged -v "$$PWD:/src" -w /src golang:1.25 \
	  sh -c 'apt-get update -qq && apt-get install -y -qq busybox-static >/dev/null && \
	         go build -o /tmp/minic ./cmd/minic && /tmp/minic info && \
	         mkdir -p /tmp/rootfs/bin && cp "$$(which busybox)" /tmp/rootfs/bin/busybox && \
	         ln -sf busybox /tmp/rootfs/bin/sh && ln -sf busybox /tmp/rootfs/bin/ls && \
	         ln -sf busybox /tmp/rootfs/bin/hostname && \
	         /tmp/minic run --rootfs /tmp/rootfs --memory 32m --pids 16 -- /bin/sh -c \
	           "echo pid=\$$\$$ ; hostname ; ls /"'

clean:
	rm -rf bin
