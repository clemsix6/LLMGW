# Build on the native builder platform, then compile the static binary for the
# requested image target. Normal builds therefore match the daemon platform;
# explicit --platform builds cross-compile coherently without emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Download modules in a separate layer so source edits don't re-fetch dependencies.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO disabled + stripped, statically linked for a distroless static base. The SQL migrations are
# embedded in the binary via //go:embed.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/llmgw ./cmd/llmgw

# The distroless runtime has no shell. Create the named-volume mountpoint here
# with the numeric non-root identity before Docker initializes the volume.
RUN install -d -m 0700 -o 65532 -g 65532 /out/cliproxy-auth

# Runtime stage: distroless static, non-root. No shell and no package manager — minimal surface.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/llmgw /usr/local/bin/llmgw
COPY --from=build --chown=65532:65532 --chmod=0700 /out/cliproxy-auth /var/lib/llmgw/cliproxy-auth
COPY THIRD_PARTY_NOTICES.md /usr/share/licenses/llmgw/THIRD_PARTY_NOTICES.md
COPY third_party/CLIProxyAPI/LICENSE /usr/share/licenses/llmgw/CLIProxyAPI-LICENSE

VOLUME ["/var/lib/llmgw/cliproxy-auth"]

ENTRYPOINT ["/usr/local/bin/llmgw"]
