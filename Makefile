GO      ?= go
PKG     := ./...
DIST    := dist

# Stamped into the binary so `ccvm version` answers "is this build current?".
# A stale binary is otherwise indistinguishable from a bug.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all
all: lint test

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: lint
lint:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt: files need formatting" && false)
	$(GO) vet $(PKG)

.PHONY: test
test:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic $(PKG)

# Test scaffolding is excluded: backendtest exists to be imported by other
# packages' tests, so counting its statements measures nothing.
.PHONY: cover
cover: test
	@grep -v "internal/backendtest" coverage.out > coverage.shipped.out
	@$(GO) tool cover -func=coverage.shipped.out | tail -1

# Integration tests are opt-in and fail — rather than skip — when a backend
# named in CCVM_ITEST_BACKENDS is unavailable. A suite that silently skips
# reports green while testing nothing.
.PHONY: itest
itest:
	$(GO) test -tags=integration -count=1 $(PKG)

# The backends CI cannot reach. Run before tagging a release.
.PHONY: itest-local
itest-local:
	CCVM_ITEST_BACKENDS=orbstack,proxmox $(MAKE) itest

# The guest binaries are the ones most likely to break unnoticed, since nothing
# on the Mac exercises them.
.PHONY: build
build:
	mkdir -p $(DIST)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/ccvm ./cmd/ccvm
	GOOS=linux GOARCH=amd64 $(GO) build -o $(DIST)/ccvm-done-linux-amd64 ./cmd/ccvm-done
	GOOS=linux GOARCH=arm64 $(GO) build -o $(DIST)/ccvm-done-linux-arm64 ./cmd/ccvm-done
	GOOS=linux GOARCH=amd64 $(GO) build -o $(DIST)/ccvm-init-linux-amd64 ./cmd/ccvm-init
	GOOS=linux GOARCH=arm64 $(GO) build -o $(DIST)/ccvm-init-linux-arm64 ./cmd/ccvm-init
	@echo "built $(DIST)/ccvm $(VERSION)"

# The image build needs the guest binaries staged into the context first: they
# are compiled from this repo rather than fetched.
IMAGE ?= ccvm/base:latest
.PHONY: image
image: build
	docker build -f profiles/base/Dockerfile -t $(IMAGE) .

.PHONY: clean
clean:
	rm -rf $(DIST) coverage.out coverage.shipped.out
