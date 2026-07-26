# CGO_ENABLED=0 is not a preference. Linking CGO ends cross-compilation and
# static linking, and with them the single-binary deployment story.
export CGO_ENABLED := 0

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all
all: check build

.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o bin/lumipanel ./cmd/lumipanel

.PHONY: check
check: fmt vet test

.PHONY: fmt
fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...

.PHONY: race
race:
	go test -race ./...

.PHONY: cover
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# Proves the single-binary claim locally, the same way CI does.
.PHONY: cross
cross:
	@for target in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do \
		echo "  $$target"; \
		GOOS=$${target%/*} GOARCH=$${target#*/} go build -o /dev/null ./cmd/lumipanel || exit 1; \
	done

.PHONY: clean
clean:
	rm -rf bin coverage.out

# Requires a reachable PostgreSQL 16 and psql. Not part of `check`, because it
# needs infrastructure; CI runs it as its own job.
.PHONY: schema
schema:
	scripts/verify-schema.sh
