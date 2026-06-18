# Build stage: cross-compile a static linux/amd64 binary. The production server is x86_64 while
# development is arm64, so the target arch is pinned explicitly rather than inherited from the host.
FROM golang:1.26 AS build

WORKDIR /src

# Download modules in a separate layer so source edits don't re-fetch dependencies.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO disabled + stripped, statically linked for a distroless static base. The SQL migrations are
# embedded in the binary via //go:embed, so the runtime image needs no files besides the binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/llmgw ./cmd/llmgw

# Runtime stage: distroless static, non-root. No shell and no package manager — minimal surface.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/llmgw /usr/local/bin/llmgw

ENTRYPOINT ["/usr/local/bin/llmgw"]
