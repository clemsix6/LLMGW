# CLAUDE.md

This file guides Claude Code when working in the **LLMGW** repository.

@~/Skills/general.md
@~/Skills/go-style.md
@~/Skills/commit-convention.md
@~/Skills/git-workflow.md
@~/Skills/feature-pipeline.md

LLMGW is a self-hosted Go gateway that **embeds the CLIProxyAPI SDK**. It adds
project-key authentication, budgets, and normalized usage accounting on top of
the SDK's provider plumbing. Everything below is what the shared standards above
do not cover.

## Embedding rule (CRUCIAL)

- One binary, one process, one container, one listener. The SDK is embedded as a
  **library** — never a child process, never a reverse-proxied sidecar.
- The SDK is consumed **unmodified**, from pristine upstream sources at the
  version pinned in `go.mod`. Never fork, patch or vendor it: it releases
  often, and upgrading must stay a plain module upgrade. A `replace` directive
  is never the answer either.
- Accounting relies on each upstream attempt reporting exactly one usage
  record. `disable-image-generation` and the empty-payload-write-rules check
  are what guarantee it — both are enforced at startup, so keep them.
- The shared YAML carries native CLIProxyAPI configuration at its root;
  LLMGW-specific configuration lives only under `llmgw`.
- Keep LLMGW secrets in environment variables or the secret manager; native
  provider credentials stay in the auth files and config keys the SDK expects.

## Ownership boundary (CRUCIAL)

The gateway owns project-key authentication, budgets, and normalized usage. The
embedded SDK owns provider protocols, provider authentication, routing,
cooldowns, and retries. Never reimplement either side inside the other.

Hexagonal, as rules rather than a file tree:

- **Domain** — governance rules and the ports they need, with no HTTP, SQL, or
  SDK import.
- **Adapters** — PostgreSQL persistence and the embedded-SDK boundary.
- **Composition root** — wiring and the local operator commands, no business
  logic.

PostgreSQL is mandatory; there is no fallback store.

## Exposed surfaces (CRUCIAL)

- Every client request carries an LLMGW project key, accepted as `Authorization:
  Bearer …` or `x-api-key`. It stays mandatory behind Cloudflare, and direct
  exposure requires TLS termination in front.
- Do not add another inbound management or API-key surface: remote management,
  control panel, home, and pprof remain disabled.

## Testing

- Every feature is covered by the **integration suite**, which drives the real
  gateway over its HTTP API against local testcontainers and stub upstreams. It
  is hermetic and is the backbone — run it, plus the domain/adapter tests, before
  committing.
- Assert on **shape and plausibility** (status, valid structure, non-empty
  content of a plausible length, expected stop reason, tool use when tools are
  used) — never exact model text.
- Stub upstreams exist for **failure injection** a real provider will not produce
  on demand: forced 429 → cooldown, exhausted pool → 503, refresh failure.
- **Retry transient upstream errors** with bounded backoff; never retry the
  gateway's own `402` / `503` — those are the assertions.
- Verify budget accounting from recorded usage: a `calls` cap is deterministic,
  token and cost limits are verified by crossing them.
- CI has no live provider credentials and must never run the live suite
  automatically. Live validation is operator-run and gated solely on
  `LLMGW_LIVE_CONFIG`.

## Container operations

- The runtime image is distroless and runs as user/group `65532:65532`.
- Keep the CLIProxy auth directory writable by that user and persistent through
  its named volume — the SDK rotates provider credentials there.
- Mount the shared `config.yaml` read-only with host mode `0600`: it can carry
  native provider credentials.
