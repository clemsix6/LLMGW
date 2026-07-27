# LLMGW CLIProxyAPI SDK Pivot Design

**Date:** 2026-07-27
**Status:** Draft for review; direction approved in design discussion

## 1. Decision

LLMGW becomes a governed distribution of CLIProxyAPI rather than maintaining its
own provider implementations.

The shipped `llmgw` binary embeds CLIProxyAPI v7 through its Go SDK. One process
and one HTTP server provide all CLIProxyAPI inference surfaces while LLMGW adds:

- project authentication through generated API keys;
- project-level budget enforcement;
- durable request and usage accounting in PostgreSQL;
- notional cost calculation;
- local administration commands for provider auth, keys, budgets, and usage.

There is no LLMGW reverse proxy in front of a child CLIProxyAPI process. LLMGW
constructs the CLIProxyAPI SDK service with its own access provider, budget
middleware, and usage plugin, then runs that service directly.

## 2. Why the Direction Changes

The current LLMGW duplicates provider behavior for Claude Max and Codex. That
creates ongoing work around OAuth, provider-specific request formats, streaming,
model discovery, retries, account rotation, cooldowns, and new provider support.

CLIProxyAPI already provides those capabilities and exposes them as a reusable Go
library:

- `sdk/cliproxy.NewBuilder` builds the complete proxy runtime;
- `Service.Run(context.Context)` owns startup and graceful shutdown;
- `WithRequestAccessManager` accepts a custom inbound authentication provider;
- `WithServerOptions` accepts middleware and server hooks;
- `Service.RegisterUsagePlugin` emits normalized usage records;
- built-in watchers reload provider credentials and native configuration.

The SDK usage records already normalize provider, model, selected account, token
breakdowns, failures, latency, time to first token, and service tier across
streaming and non-streaming protocols. LLMGW therefore does not need to parse
Claude SSE, Chat Completions, Responses, Gemini, or future provider wire formats
for accounting.

## 3. Goals

- One binary, one process, one container image, and one public HTTP listener.
- Preserve the complete inference surface supported by the pinned CLIProxyAPI SDK.
- Use standard client authentication fields supported by Claude Code, Hermes,
  OpenCode, OpenAI-compatible clients, and Anthropic-compatible clients.
- Attribute every authenticated generation request to one project.
- Allow multiple independently revocable keys for a project, including overlap
  during rotation.
- Enforce project-level budgets and record calls, tokens, failures, and notional
  cost.
- Count one client request once even when CLIProxyAPI performs multiple upstream
  attempts.
- Keep provider credentials and routing under CLIProxyAPI's native mechanisms.
- Keep administration local; expose no LLMGW management HTTP API.
- Fail closed when authentication or budget state cannot be read safely.

## 4. Non-goals

- Arbitrary request tags.
- Project identity selected by a header, port, hostname, path, or model.
- Per-key budgets in the first version. Keys are an attribution dimension; the
  enforced budget belongs to the project.
- Per-project model allowlists in the first version. A valid project key can use
  the models exposed by CLIProxyAPI.
- A web administration interface.
- A remotely accessible management API.
- Reimplementing provider OAuth, routing, translation, retry, or cooldown logic.
- Running CLIProxyAPI as a child process or sidecar.
- Strict zero-overshoot token or cost limits. Output usage is unknowable before a
  generation completes; the precise semantics are defined in section 9.
- Multi-instance LLMGW coordination in the first version. One active LLMGW
  process owns one PostgreSQL database and one CLIProxyAPI auth directory.

## 5. Runtime Architecture

```text
Claude Code / Hermes / OpenCode / SDK
                  |
                  | standard Authorization or x-api-key
                  v
        CLIProxyAPI HTTP server
        embedded in the llmgw process
                  |
       +----------+-----------+
       |                      |
       v                      v
LLMGW request guard      CLIProxyAPI runtime
- authenticate key      - protocol handlers
- resolve project       - translation
- budget pre-check      - account selection
- create request row    - retry and cooldown
       |                - streaming
       |                      |
       +----------+-----------+
                  |
                  v
        LLMGW UsagePlugin
        - normalized attempts
        - token accounting
        - price calculation
        - PostgreSQL persistence
```

`llmgw serve` performs the following startup sequence:

1. Load and validate configuration.
2. Open PostgreSQL and apply LLMGW migrations.
3. Require the API-key pepper and validate the CLIProxyAPI auth directory.
4. Construct the LLMGW access provider and request guard.
5. Load the native CLIProxyAPI SDK configuration.
6. Build the CLIProxyAPI service with the LLMGW components.
7. Register the LLMGW usage plugin.
8. Run the SDK service on the configured listener.

If configuration, PostgreSQL, or the SDK service cannot start, no inference
listener remains active. Cancellation of the root context gracefully stops the
SDK service, drains usage handling, and then closes PostgreSQL.

The request guard is installed as global SDK middleware, before CLIProxyAPI's
route-level access middleware. It authenticates the project key, resolves the
project, performs admission, creates the request row, and attaches an immutable
request identity to `http.Request.Context()`. The custom SDK access provider is
then only a bridge: it accepts an already prepared LLMGW identity and returns the
non-secret key public ID as the SDK principal. It never performs a second,
independent authentication path.

Before `Builder.Build`, LLMGW registers its provider under a dedicated SDK access
type and calls `sdk/access.SetExclusiveProvider` for that type. This matters
because the builder repopulates even a caller-supplied access manager from the
SDK registry. The resulting manager therefore contains only the LLMGW provider,
including after a native configuration reload. A non-empty CLIProxyAPI
`api-keys` configuration is also a startup validation error, preventing a native
key from bypassing project attribution or budgets.

The CLIProxyAPI dependency is pinned to `v7.2.102` initially
(`8423cce2d1004e80948a9e2c60ee69354c0aabc3`). Upgrades are explicit changes and
must pass the complete LLMGW integration suite. All SDK imports remain inside a
focused CLIProxy adapter package so upstream API churn does not leak into the
budget and project domains.

## 6. Public API and Compatibility

LLMGW exposes a single base URL, for example:

```text
https://llmgw.example.com
```

It preserves the inference routes registered by the pinned CLIProxyAPI SDK,
including the following protocol families:

- Anthropic Messages;
- OpenAI Chat Completions;
- OpenAI Responses;
- Gemini-compatible routes;
- model discovery;
- streaming and non-streaming variants;
- compatible image, video, realtime, and direct Codex routes exposed by the SDK.

The wrapper does not maintain a duplicate inference route table. Compatibility
follows the pinned SDK, with LLMGW integration tests guarding the routes its
consumers use.

`GET /healthz` is the only unauthenticated route on `llmgw serve` and contains no
sensitive state. CLIProxyAPI's remote management API and management panel are
disabled. Provider OAuth callbacks are handled by the local `llmgw auth login`
command on a temporary loopback listener with state validation; they are not
part of the public server surface.

Every other request requires a valid LLMGW project key. A small explicit
metadata allowlist covers model discovery and count-token endpoints.
Authenticated requests outside that allowlist default to generation requests
and consume one call, including routes added by a future SDK version until
LLMGW classifies them. This fail-safe default prevents a new route from
accidentally bypassing budget tracking without copying the SDK's complete route
table.

Cloudflare is a deployment layer, not a runtime dependency. The recommended
deployment publishes one hostname through Cloudflare Tunnel to the single LLMGW
listener. No project-specific hostname or port is required. WARP or additional
Cloudflare controls can be added without changing LLMGW identity semantics.

## 7. Project Keys

Each API key belongs to exactly one project. A project may have several keys so
different machines can be revoked independently and rotations can overlap.

Example:

```text
truewallet
  key "server-1-claude"
  key "server-2-hermes"
  key "server-2-opencode"
```

All three keys consume the same `truewallet` budget. Reports may group usage by
key name without introducing the old free-form tag model.

### 7.1 Key format and storage

A generated key has three opaque parts:

```text
llmgw_k_<public-id>_<secret>
```

- `<public-id>` is a non-secret lookup identifier containing at least 12 random
  bytes.
- `<secret>` contains 32 cryptographically random bytes encoded with unpadded
  base64url.
- The full value is displayed once by the CLI.
- PostgreSQL stores the public ID and
  `HMAC-SHA-256(LLMGW_KEY_PEPPER, full-key)`, never the full key.
- Validation recomputes the HMAC and compares it in constant time.
- `LLMGW_KEY_PEPPER` is required at startup and comes from the process
  environment or the deployment's secret manager.
- The pepper contains at least 32 random bytes and must remain stable across
  restarts and database restores. Losing or changing it invalidates every
  existing key; transparent pepper rotation is outside the first version.

The key row stores:

- project ID;
- operator-visible name;
- public ID and digest;
- creation and optional expiry timestamps;
- revocation timestamp;
- last-used timestamp.

No request, error, debug log, or usage record may contain a raw project key.

### 7.2 Accepted client headers

The request guard accepts:

```http
Authorization: Bearer llmgw_k_...
```

or:

```http
x-api-key: llmgw_k_...
```

If both are present, their values must match exactly. Unsupported authorization
schemes, malformed keys, unknown keys, expired keys, and revoked keys return the
same generic `401` response to avoid credential enumeration.

After validation, the SDK access provider returns the non-secret key public ID as
the CLIProxyAPI principal. The SDK consequently places only that identifier, not
the credential, in normalized usage records.

The request guard also places an immutable LLMGW request identity in the Go
request context. It includes request ID, project ID, and key ID. The SDK preserves
the parent request context when it executes providers and emits usage, allowing
the usage plugin to correlate all upstream attempts with the originating client
request.

## 8. Request and Attempt Model

LLMGW distinguishes a client request from CLIProxyAPI's upstream attempts.

```text
request 019...
  project=truewallet
  key=server-1-claude
  requested_model=claude-opus

  attempt A
    account=claude-account-a
    result=429

  attempt B
    account=claude-account-b
    result=success
    input_tokens=10,200
    output_tokens=2,230
```

One generation request consumes one `calls` unit. Each SDK usage record becomes
a separate attempt attached to that request. Token and cost totals sum all
attempts that report usage, including failed attempts that consumed tokens.

Model discovery, health checks, local OAuth, and local management operations are
not generation requests and do not consume budget. Model discovery and
count-token endpoints are authenticated and recorded as metadata operations but
do not consume a generation call.

For a generation route, the request guard creates the client request row before
handing control to the CLIProxyAPI handler. This row is the durable in-flight
call reservation; a separate reservation table is no longer needed for call
limits. Metadata operations also receive a request row, marked
`accounting_state=not_applicable`, but do not participate in admission totals.
The middleware records the final downstream HTTP status and completion timestamp
after the SDK handler returns.

## 9. Budget Semantics

Budgets remain project-level and support:

- dimensions: `calls`, `tokens`, and `cost`;
- windows: `hour` and `day`;
- actions: `block` and `warn`.

The `hour` and `day` windows are rolling 60-minute and rolling 24-hour windows
over UTC timestamps, not calendar buckets.

The pre-request transaction takes a project-scoped PostgreSQL advisory lock,
reads the applicable totals, evaluates all limits, and inserts the in-flight
request row atomically when allowed.

### 9.1 Calls

Call limits are strict. Because the request row is inserted atomically before
dispatch, concurrent requests cannot jointly pass a remaining single-call
allowance. Authenticated generation requests count even when the upstream later
fails; retries do not add client calls.

### 9.2 Tokens and cost

Token and cost totals use completed SDK usage attempts. Before starting a new
request, LLMGW blocks when the finalized total has reached or exceeded a blocking
limit.

An accepted request may cross a token or cost limit because its output is not
known in advance. Concurrent in-flight requests may also complete beyond the
threshold. Therefore token and cost limits have explicit **stop-after-crossing**
semantics, not a zero-overshoot guarantee. Once the crossing usage is persisted,
no further request is admitted until the active window falls below the limit.

This is the KISS behavior for the first version. Conservative maximum-output
reservations or per-project concurrency caps require a separate design because
they trade availability for a tighter overshoot bound.

### 9.3 Responses

- Missing, malformed, expired, or revoked key: `401`.
- Blocking budget limit reached: `402` with a stable
  `budget_exceeded` error code and no internal totals beyond the relevant
  dimension and reset time.
- Authentication or budget store unavailable: `503`; requests fail closed.
- Native CLIProxyAPI provider, cooldown, and retry errors pass through unchanged.

Warnings are recorded and logged but do not alter the provider response.

## 10. Usage and Cost Accounting

LLMGW registers a CLIProxyAPI `usage.Plugin`. Its `HandleUsage` implementation
consumes normalized SDK records containing:

- provider and concrete executor type;
- resolved model and client-requested alias;
- selected upstream auth ID and auth type;
- input, output, reasoning, cache-read, and cache-creation tokens;
- total tokens;
- success or failure and upstream status;
- request time, total latency, and time to first token;
- requested and response service tiers.

LLMGW intentionally does not store `Source` values that may contain upstream
account emails. It stores the SDK's opaque upstream auth ID for operational
diagnostics.

The plugin calculates notional cost from LLMGW's price table using the resolved
provider, model, service tier, and normalized token breakdown. Subscription-backed
Claude and Codex usage remains notional; it is not presented as an invoice.
Reasoning tokens are retained as a diagnostic breakdown and are not added a
second time when the provider already includes them in output tokens.

Unknown pricing stores the token usage with a null cost and an
`unknown_pricing` state. If a project has an active blocking cost limit, new
requests fail closed while unknown-priced usage exists in that limit's window.
Token and call limits continue to work independently.

The SDK dispatches usage plugins asynchronously relative to the client response.
`HandleUsage` preserves the identity values from the request context, detaches
from request cancellation, and performs the PostgreSQL write synchronously
inside the callback with a short deadline and bounded retry. Graceful service
shutdown drains the SDK usage queue before PostgreSQL closes.

The durable request row guarantees the client-call count even if accounting is
interrupted. A reconciler waits 30 seconds after request completion: a generation
request with at least one persisted attempt becomes `observed`, while one with
no attempt becomes `accounting_unknown`. Stale in-flight requests are also
marked `accounting_unknown` after the recovery grace period. Projects with
blocking token or cost limits fail closed while a relevant request is
`accounting_unknown`. Ordinary in-flight `pending` requests remain allowed, as
required by the stop-after-crossing concurrency semantics. The operator resolves
an unknown request explicitly with:

```bash
llmgw usage resolve <request-id> --assume-zero
```

This favors budget safety over silently undercounting after an abnormal crash.

There is one explicit residual limitation in the current SDK contract. An abrupt
process death can occur after some, but not all, asynchronous attempt records
have been persisted. Because `usage.Plugin` has no end-of-request
acknowledgement, LLMGW can detect a completely missing attempt set but cannot
prove that an observed set is complete. Such a crash may therefore undercount
tail attempts. The integration and chaos suites guard the known behavior, but
this design does not claim zero-loss token or cost accounting. A durable outbox
or SDK lifecycle extension would require a separate design.

## 11. Data Model

The target PostgreSQL model is:

```text
project(
  id, name UNIQUE, created_at
)

client_key(
  id, project_id, name,
  public_id UNIQUE, digest,
  created_at, expires_at, revoked_at, last_used_at
)

budget_limit(
  id, project_id,
  dimension, window, max_value, action,
  created_at, updated_at
)

request_event(
  id UUID, project_id, client_key_id,
  requested_at, completed_at,
  method, path, requested_model,
  state, accounting_state, downstream_status
)

usage_attempt(
  id UUID, request_id,
  provider, executor_type,
  resolved_model, requested_alias,
  upstream_auth_id, upstream_auth_type,
  input_tokens, output_tokens, reasoning_tokens,
  cache_read_tokens, cache_creation_tokens, total_tokens,
  service_tier, response_service_tier,
  failed, upstream_status,
  latency_ms, ttft_ms,
  cost_usd, pricing_state,
  created_at
)

model_price(
  provider, model_pattern, service_tier,
  input_per_million, output_per_million,
  cache_read_per_million, cache_creation_per_million,
  effective_from
)
```

The request ID is generated before dispatch and carried through the request
context. Attempt IDs are generated once inside `HandleUsage` and reused across
that handler's database retries. `accounting_state` is one of `pending`,
`observed`, `accounting_unknown`, or `not_applicable`; `observed` means at least
one attempt was persisted, not that the SDK supplied a completeness proof.

Indexes support windowed totals by project and timestamp, request-to-attempt
lookup, public-key lookup, and unresolved request recovery.

## 12. Configuration

LLMGW uses one YAML file. Native CLIProxyAPI fields remain at the root, so the
full upstream configuration surface remains available without LLMGW copying its
schema. LLMGW owns one namespaced block:

```yaml
host: 127.0.0.1
port: 8088
auth-dir: ~/.llmgw/cliproxy-auth

remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true

request-retry: 1
max-retry-credentials: 2
max-retry-interval: 5
transient-error-cooldown-seconds: -1
disable-cooling: false
save-cooldown-status: false

routing:
  strategy: round-robin
  session-affinity: false

llmgw:
  postgres-dsn-env: LLMGW_POSTGRES_DSN
  key-pepper-env: LLMGW_KEY_PEPPER
  usage-retention-days: 35
```

The pinned SDK currently ignores unknown YAML fields, allowing it to load the
same file while LLMGW reads the `llmgw` block. An integration test pins this
behavior. A later SDK switching to strict decoding is treated as a breaking
upgrade: LLMGW must adapt both initial parsing and the SDK watcher before that
version can be accepted. The operator-facing file remains a single file.

Raw project keys, the pepper, PostgreSQL credentials, and provider credentials
are never stored in this file. Provider OAuth material remains in CLIProxyAPI's
native auth directory. LLMGW's custom request access manager replaces
CLIProxyAPI's public `api-keys` list. The `api-keys` field must be absent or
empty; a non-empty value fails startup validation.

Changes to native CLIProxyAPI fields and auth files follow the SDK's hot-reload
behavior. Changes to the `llmgw` block require a graceful restart in the first
version.

### 12.1 Recommended account-pool policy

The example values intentionally separate provider quota from transient server
errors:

- `round-robin` distributes new requests across currently eligible accounts;
- one request may try at most two credentials and perform one bounded retry;
- provider quota or rate-limit signals may cool only the affected
  account/model, because `disable-cooling` remains false;
- generic transient `408`/`5xx` responses do not create a 60-second account
  quarantine, because `transient-error-cooldown-seconds` is `-1`;
- cooldown state stays in memory and is not resurrected from `.cds` files after
  a restart;
- auth-file hot reload makes a newly added or refreshed account eligible without
  restarting LLMGW.

These are safe initial operational defaults, not hard-coded policy. Operators
may tune them in the native configuration, and LLMGW's account-pool chaos tests
must pass for the supported production profile. LLMGW never creates its own
permanent blacklist beside the SDK's native eligibility and cooldown state.

## 13. One Binary: Server and Local CLI

The same executable provides runtime and administration:

```text
llmgw [serve]
llmgw auth login <provider>
llmgw auth list
llmgw auth import-legacy
llmgw key create <project> --name <client> [--expires <duration>]
llmgw key list [project]
llmgw key rotate <key-id> [--overlap <duration>]
llmgw key revoke <key-id>
llmgw budget set <project> --dimension <calls|tokens|cost>
                              --window <hour|day>
                              --max <value>
                              --action <block|warn>
llmgw budget list [project]
llmgw budget delete <limit-id>
llmgw usage show <project> [--since <duration>] [--by key|model|provider]
llmgw usage resolve <request-id> --assume-zero
```

Running `llmgw` without a subcommand remains an alias for `llmgw serve`.

`key create` creates the project if absent. Administrative commands connect
directly to PostgreSQL or use the CLIProxyAPI auth SDK locally; they do not call a
remote management API. In Docker, operators use the same image:

```bash
docker compose exec llmgw llmgw key create truewallet --name server-1
```

Rotation creates a replacement key before revoking the old key. With an overlap,
the old key receives an `expires_at` timestamp and remains valid until then;
without an overlap it receives `revoked_at` immediately.

## 14. Failure Handling

- PostgreSQL unavailable during startup: do not listen.
- PostgreSQL unavailable during authentication or budget check: return `503`.
- Usage write temporarily fails: retry without blocking the client response; new
  requests fail closed because their budget pre-check also requires PostgreSQL.
- Process terminates with accepted requests in flight or with no persisted usage
  attempt: recover them as `accounting_unknown` after the applicable grace
  period.
- Process terminates after only part of a multi-attempt sequence is persisted:
  retain the observed attempts; the missing tail cannot be detected or
  reconstructed from the current SDK callback contract.
- Client disconnects mid-stream: retain the client call and persist any usage
  emitted by the SDK.
- CLIProxyAPI cannot select an account: preserve its status, body, and
  `Retry-After`.
- CLIProxyAPI service returns unexpectedly: terminate `llmgw` so systemd or
  Docker restarts the whole unit.
- Usage plugin panic: the SDK recovers the panic; LLMGW also emits a critical
  metric and leaves the request recoverable as unknown accounting.

## 15. Security Requirements

- Project identity comes only from a validated key.
- `X-Project` and `X-Tags` are removed and ignored; they can never override key
  identity.
- Project keys use a CSPRNG, are displayed once, stored as keyed digests, compared
  in constant time, and redacted from logs.
- Provider credentials remain owned by CLIProxyAPI and are never returned through
  LLMGW usage or CLI output.
- The remote management API and control panel are disabled.
- Local administration requires shell access to the LLMGW host/container and its
  configured secrets.
- Authentication failures use indistinguishable responses.
- Request body logging stays disabled by default because prompts and tool payloads
  may contain sensitive data.
- Cloudflare Tunnel is the recommended exposure mechanism so the origin has no
  public inbound port. LLMGW keys remain required even behind Cloudflare.
- Deployments without Cloudflare must still terminate TLS before the LLMGW
  listener; project keys must never cross an unencrypted network.
- The CLIProxyAPI MIT license and copyright notice ship with LLMGW distributions.

## 16. Migration from the Current LLMGW

The pivot deliberately removes public behavior that no longer fits the model:

- remove `X-Project`, `X-Tags`, and `LLMGW_DEFAULT_PROJECT`;
- stop auto-creating projects from HTTP requests;
- retire the native Claude Max and Codex provider adapters after SDK parity tests;
- stop using LLMGW's provider, route, and OAuth-token storage for live traffic;
- replace per-`(project, tag)` enforcement with project-only enforcement;
- replace protocol-specific response parsing with the SDK usage plugin.

Existing project rows are retained. The migration renames the old `usage_event`
table to `legacy_usage_event`, where historical rows remain queryable without a
`client_key_id`. It renames the old `budget_limit` table to
`legacy_budget_limit`, creates the project-only replacement, and copies only
rows whose old `tag` is null. Tag-specific rows remain archived and are not
silently merged into a project-wide cap. The obsolete `reservation` table is
removed only during a stopped-traffic migration, after no reservation can still
represent an active call.

`llmgw auth import-legacy` imports supported existing provider credentials into
the CLIProxyAPI auth directory and reports accounts that require a fresh
`llmgw auth login <provider>`. It is idempotent and never deletes the source
credential rows. Native adapters are removed only after the import and live
parity tests succeed for every provider/protocol combination currently used in
production.

## 17. Verification Strategy

The suite is integration-first. It does not mock, duplicate, or try to unit-test
CLIProxyAPI internals. Small unit tests cover only deterministic LLMGW logic;
contracts at the proxy boundary run against the real pinned SDK.

### Small pure unit tests

- key format parsing, digesting, and constant-time comparison;
- project-level budget arithmetic;
- cost calculations for input, output, reasoning, and cache tokens;
- pure mapping from an SDK usage value to an attempt row.

### Embedded integration tests

Run the real pinned CLIProxyAPI SDK, deterministic stub upstream executors, and
ephemeral PostgreSQL containers:

- Anthropic Messages, OpenAI Chat Completions, Responses, and Gemini routes;
- streaming and non-streaming usage parity;
- model discovery authenticated but unmetered;
- unknown SDK routes default to authenticated, metered generation operations;
- one client call with a failed attempt followed by a successful account;
- calls counted once and tokens summed across attempts;
- `Authorization` and `x-api-key` compatibility;
- key expiry, revocation, and conflicting authentication headers;
- exact call-limit behavior under concurrency;
- token/cost stop-after-crossing behavior;
- unknown-pricing fail-closed behavior;
- client cancellation and malformed requests;
- PostgreSQL loss during auth, pre-check, and usage persistence;
- graceful shutdown and stale in-flight recovery;
- legacy schema and provider-auth migration;
- management routes unavailable;
- config hot reload cannot replace the exclusive LLMGW access provider;
- logs contain no raw project or provider credentials;
- request context identity survives through the SDK usage plugin.

### Account-pool chaos tests

Exercise the current CLIProxyAPI version against controlled errors:

- 429 without reset metadata;
- provider usage limit with reset metadata;
- global 503;
- network failure;
- all accounts cooling;
- new account added while the existing pool is cooling;
- streaming failure before and after the first response payload.

These tests pin the operational assumptions around
`max-retry-credentials`, transient cooldowns, retry timing, and scheduler refresh.

### Gated live tests

Use real provider accounts only in manually gated tests. Verify OAuth refresh,
one non-streaming request, one streaming request, normalized usage, and account
failover without placing credentials or prompt content in CI logs.

## 18. Resulting Product

The operator installs and starts LLMGW. They do not separately install, launch,
or expose CLIProxyAPI.

```text
llmgw serve
```

Consumers use one URL and a standard API-key field. The key selects the project,
LLMGW enforces and records governance, and the embedded CLIProxyAPI SDK owns the
provider complexity.

This keeps the product boundary simple:

> CLIProxyAPI is the engine; LLMGW is the secure, budget-aware product.
