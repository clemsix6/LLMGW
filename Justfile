set quiet

# Formatting check — the same gate CI enforces.
fmt:
    #!/usr/bin/env bash
    set -euo pipefail
    unformatted="$(gofmt -l cmd internal test)"
    if [ -n "$unformatted" ]; then
        echo "gofmt needed on:"; echo "$unformatted"; exit 1
    fi

# Static analysis.
vet:
    go vet ./...

# Build every package.
build:
    go build ./...

# Domain and adapter tests — no Docker required.
test-unit:
    go test -race -count=1 ./internal/...

# Embedded-SDK integration suite — requires Docker for testcontainers.
test-integration:
    go test -count=1 ./test/integration

# Hermetic gates, in the order CI runs them.
test: fmt vet build test-unit test-integration

# Operator-owned live run — needs an isolated LLMGW_LIVE_CONFIG, never production credentials.
test-live:
    go test ./test/e2e -count=1 -v

# Runtime image, then the non-root/auth-dir/license checks it must satisfy.
docker-verify tag="llmgw:local":
    docker build --tag {{tag}} .
    scripts/verify-runtime-image.sh {{tag}}
