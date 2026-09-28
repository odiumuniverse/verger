BINARY_NAME=verger
GO=go
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS=-ldflags "-X main.version=$(VERSION)"

# A self-signed code signing identity pins the designated requirement to the
# certificate instead of the cdhash, so it is stable across rebuilds. Without it the
# build falls back to ad-hoc. The identifier matters too: codesign defaults a Go binary
# to "a.out".
SIGN_IDENTITY?=odiumuniverse code signing
SIGN_ID?=com.odiumuniverse.$(BINARY_NAME)

.PHONY: all build sign clean test test-short test-runtime runtime lint lint-fix fmt vet run mod audit pre-commit help

all: fmt lint test build

build:
	@echo "Building $(BINARY_NAME)..."
	$(GO) build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/verger

## codesign the built binary, ad-hoc when the identity is absent
sign: build
	@if codesign --force --sign "$(SIGN_IDENTITY)" --timestamp=none --identifier $(SIGN_ID) bin/$(BINARY_NAME) 2>/dev/null; then \
	  :; \
	else \
	  echo "warning: cannot sign with '$(SIGN_IDENTITY)'; signing ad-hoc"; \
	  codesign --force --sign - --identifier $(SIGN_ID) bin/$(BINARY_NAME); \
	fi
	codesign --verify --strict bin/$(BINARY_NAME)
	@echo "signed bin/$(BINARY_NAME): $$(codesign -dvvv bin/$(BINARY_NAME) 2>&1 | grep -m1 Authority | cut -d= -f2- || echo ad-hoc)"

clean:
	rm -rf bin/ coverage.out coverage.html

test:
	$(GO) test -race ./...

test-short:
	$(GO) test -short ./...

## rebuild the host runtime bundle (runtime/ -> pkg/runtime/bundle, table)
## the bundle is committed: `go build` and `go test` never need node
runtime:
	cd runtime && npm ci --no-audit --no-fund && npm run build

## the runtime bundle's own tests: they run the built artifact under node
test-runtime:
	cd runtime && npm ci --no-audit --no-fund && npm test

fmt:
	golangci-lint fmt ./cmd/... ./pkg/...

lint:
	golangci-lint run ./cmd/... ./pkg/...

lint-fix:
	golangci-lint run --fix ./cmd/... ./pkg/...

vet:
	$(GO) vet ./...

run: build
	./bin/$(BINARY_NAME)

mod:
	$(GO) mod tidy
	$(GO) mod vendor

audit:
	govulncheck ./...

pre-commit: fmt lint test

help:
	@printf '  %-11s %s\n' \
		all 'Format, lint, test and build' \
		build 'Build the application' \
		clean 'Clean build artifacts' \
		test 'Run all tests with race detector' \
		test-short 'Run tests without long-running ones' \
		test-runtime 'Test the host runtime bundle under node' \
		runtime 'Rebuild the host runtime bundle and table' \
		lint 'Run linter' \
		lint-fix 'Run linter and auto-fix issues' \
		fmt 'Format code' \
		vet 'Run go vet' \
		run 'Build and run the application' \
		mod 'Tidy modules and vendor dependencies' \
		audit 'Audit dependencies for known vulnerabilities' \
		pre-commit 'Format, lint and test' \
		help 'Show this help message'
