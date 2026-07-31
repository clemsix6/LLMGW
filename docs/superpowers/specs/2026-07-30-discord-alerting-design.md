# Discord Alerting Design

**Date:** 2026-07-30
**Status:** Draft

## 1. Decision

LLMGW gains one outbound notification channel: a Discord webhook that reports
state changes an operator must act on. Provider credentials that hit their quota
or stop authenticating, generations that start failing, project keys that are
about to expire, budgets that start blocking a project, and gateway health
transitions each produce one Discord message when — and only when — the
underlying state changes.

The channel is outbound-only. It adds no inbound surface, no management API, and
no new listener. It is optional: with no webhook URL configured, the gateway
behaves exactly as it does today.

## 2. Why

Today every one of these facts reaches a `log.Print` and nothing else. An
operator learns that a Claude credential stopped refreshing when a project
complains, learns that a budget blocks traffic when a client reports `402`, and
learns that a project key expired after it already expired. Most of the evidence
already flows through the process — request admission carries budget breaches,
the SDK usage plugin carries every upstream attempt with its credential, model
and HTTP status — but nothing pushes it anywhere an operator watches.

## 3. Goals

- One Discord message per state transition, never per occurrence.
- Cover provider credentials, generation health, project keys, budgets, and
  gateway health.
- Never block, slow, or fail a client request because of the notifier.
- No schema migration, no new persistent state, no new inbound surface.
- Stay inside the ownership boundary: the SDK is consumed unmodified, and every
  observation uses a seam the SDK already offers.

## 4. Non-Goals

- No daily or periodic digest. Transitions only.
- No second delivery channel (Slack, e-mail, SMS) and no per-severity routing.
- No persistence of notification state across restarts.
- No alert-management commands (no mute, no acknowledge, no history).
- No advance warning before a provider credential dies (§13).
- No alert on rejected client credentials (§13).

## 5. Architecture

Three pieces, along the repository's existing hexagonal boundary.

**Domain — `internal/domain/governance/alert`.** Event types, severities, the
`Notifier` port, and the transition engine that decides whether an observed fact
is a state change. No HTTP, no SQL, no SDK import. This is where the behaviour
worth testing lives.

**Adapter — `internal/adapter/discord`.** One `Webhook` type implementing
`alert.Notifier`: bounded queue, single delivery goroutine, embed rendering,
bounded retry honouring Discord's `Retry-After`, and a draining `Close`.

**Composition — `internal/command`.** `runServeWith` owns construction and
lifetime (§10). The operator commands build their own short-lived pair for the
events they emit (§11).

The SDK is observed only through seams it already exposes: the extra
`sdkusage.Plugin` slot of `cliproxy.NewService`, and the middleware LLMGW
already installs. Nothing new is asked of the SDK.

### 5.1 Domain surface

```go
// Kind identifies one notification-worthy state change.
type Kind string

// Severity classifies how urgently an event needs an operator.
type Severity string

const (
    SeverityCritical Severity = "critical"
    SeverityWarning  Severity = "warning"
    SeverityInfo     Severity = "info"
)

// Field is one labelled value rendered beside an event.
type Field struct {
    Name  string // Name labels the value.
    Value string // Value is the already-safe rendered value.
}

// Event is one notification-worthy state change.
type Event struct {
    Kind     Kind      // Kind identifies the change.
    Severity Severity  // Severity is derived from Kind.
    Summary  string    // Summary is the one-line human description.
    Fields   []Field   // Fields carry the identifying context.
    At       time.Time // At is the UTC observation time.
}

// Notifier delivers events without blocking its caller.
type Notifier interface {
    // Notify reports whether the event was accepted for delivery.
    Notify(Event) bool
}

// Tracker converts observed facts into state transitions.
type Tracker struct { /* unexported state */ }
```

`Kind` values are the identifiers in the §6 tables (`credential_unauthorized`,
`budget_blocked`, …). They are stable: they appear in messages and in tests.
Severity is a fixed property of the kind, held in one table inside the package.

`Notify` returns whether the event entered the delivery queue, which is what
lets the tracker avoid losing an alert when the queue is saturated (§5.2).

`Tracker` is safe for concurrent use. Its observation methods are the only
public surface besides construction:

| Method | Caller | Purpose |
|---|---|---|
| `ObserveAttempt(provider, credentialID, model string, failed bool, status int)` | alert usage plugin | credential transitions |
| `ObserveGeneration(status int)` | middleware | generation health |
| `ObserveAdmission(project string, blocks, warnings []governance.BudgetBreach)` | middleware | budget transitions |
| `ObserveProjectKeys(keys []governance.KeyInfo, now time.Time)` | worker | key expiry transitions |
| `ObserveDatabase(healthy bool)` | middleware, workers | database availability |
| `Emit(kind Kind, fields ...Field)` | composition root, operator commands | one-shot lifecycle and operator events |

A tracker built with a nil notifier is a no-op, which is how the disabled
configuration is expressed: no branch at any call site. The anti-flap window
(§5.2) is a construction parameter so the integration suite can set it to zero.

Five rules pin down the observation contract:

- **Severity is a property of the kind**, not of the call site. The tracker
  builds the whole `Event`, so callers never choose a severity. Operator
  commands therefore go through `Tracker.Emit` too, never `Notifier.Notify`.
- **Attempts are observed through the SDK's extra-plugin seam.**
  `cliproxy.NewService` already accepts additional `sdkusage.Plugin` values and
  wraps them in `nonBarrierUsagePlugin`, which strips the usage bridge's barrier
  and cancellation control records. A thin `alertUsagePlugin` passed there sees
  exactly the real upstream attempts and nothing else. The existing
  `UsagePlugin` is not modified.
- **`ObserveAdmission` sees generation admissions only.** Metadata requests are
  recorded with an empty always-allowed `Admission`, so observing them would
  clear every budget state for the project (§6.4).
- **`ObserveGeneration` sees admitted generations only.** It is called from the
  middleware's completion path, which a request reaches only after it was
  admitted and handed to the SDK — LLMGW's own `402` and `503` abort earlier and
  never reach it. The status it reports is therefore the SDK's, not the
  gateway's.
- **A cancelled caller is not a database failure.** `ObserveDatabase(false)` is
  called only when the *caller's* context is still live (`ctx.Err() == nil`).
  The error's shape is not the test: a hanging PostgreSQL surfaces as
  `DeadlineExceeded` and must page the operator, while a client hang-up must
  not — and `Middleware.complete` runs under `context.WithoutCancel`, so its
  deadline can only mean genuine slowness.

### 5.2 Transition semantics

The tracker holds one state per watched entity:

- **credential** — keyed by (SDK auth ID, model), because the SDK cools down per
  credential *and* model; states `ok`, `rate_limited`, `unauthorized`,
  `failing`.
- **generation health** — one global state; `ok` or `failing`.
- **budget** — keyed by project + dimension + window + action, matching the
  uniqueness of `budget_limit`, so a `warn` and a `block` rule on the same
  dimension cannot overwrite one another; states `ok`, `warned`, `blocked`.
- **project key** — keyed by the key's public ID; states `ok`, `expiring`,
  `expired`, and never moves backwards.
- **database** — one global state; `up` or `down`.

An observation that leaves the state unchanged emits nothing. A state change
emits one event.

A minimum re-notification interval of **15 minutes** per (entity, kind)
suppresses flapping. Two rules keep it from swallowing what matters:

- an **escalation in severity passes the window from a degraded state**, so a
  credential that goes rate-limited (warning) then unauthorized (critical)
  delivers both. It does not apply from a healthy delivered state: allowing it
  would disarm the window for the commonest flap shape — degraded, recovered,
  degraded again within minutes — which is precisely the case the window exists
  for. Nothing is lost either way, since suppression defers rather than
  discards;
- **suppression defers, it never discards.** The tracker records the last state
  it actually delivered, separately from the current state. On any later
  observation of that entity, if the current state still differs from the
  delivered one and the window has elapsed, the event is emitted then. The same
  mechanism covers a saturated queue: `Notify` returning false leaves the
  delivered state untouched, so the next observation retries. An alert can be
  late; it cannot be silently lost while traffic continues.

State lives in memory. A restart resets it, so a still-degraded credential is
reported once more after the gateway comes back — which is coherent, because the
restart itself is notified.

### 5.3 Credential labels

Usage records carry `UpstreamAuthID`, not the operator-facing label, and an ID
like `claude-legacy-…-a1b2c3.json` is poor material for an alert. At startup —
after `serve` has already prepared the auth directory — `cliproxy.ListAuth`
yields ID, provider, label and disabled flag for every local auth file, and the
tracker uses it as a lookup table for rendering. A credential absent from it is
named by its ID.

Reading it is best-effort: alerting is optional and must not be able to stop the
gateway. A failed `ListAuth` logs a warning and yields an empty table.

The inventory is **not** used as a denominator for anything. An earlier revision
derived a `provider_exhausted` alert from "all known credentials degraded"; that
was dropped because the population is not knowable (YAML-declared API keys are
invisible to `ListAuth`, disabled entries distort the count, and a credential
killed by the background refresh loop never appears in any observation at all).
`generation_failures` (§6.2) covers the same operator need without a
denominator.

## 6. Alert Catalogue

### 6.1 Provider credentials

Observed from upstream attempts, keyed by (credential, model).

| Kind | Severity | Trigger |
|---|---|---|
| `credential_unauthorized` | critical | attempt returns 401 or 403 — the OAuth refresh is dead, the account must be re-logged |
| `credential_rate_limited` | warning | attempt returns 429 — quota reached, the SDK falls back elsewhere |
| `credential_failing` | warning | attempt fails with a 5xx or with no status at all (transport failure) |
| `credential_recovered` | info | first success for that credential and model after a degraded state |

Fields: provider, credential label (or ID), model, upstream status. A transport
failure reports no HTTP status — the SDK maps it to zero — so the field is
omitted and the summary says so.

Client-caused upstream failures are **not** credential problems: 4xx statuses
other than 401, 403 and 429 (a malformed body, an unknown model, an oversized
request) leave credential state untouched. Otherwise one project's bad requests
would page the operator about credentials that are perfectly healthy.

### 6.2 Generation health

| Kind | Severity | Trigger |
|---|---|---|
| `generation_failures` | critical | three consecutive admitted generations complete with a 5xx or a 429 |
| `generation_recovered` | info | first admitted generation served afterwards |

A **429 counts as a failure**: the SDK answers an exhausted credential pool that
way, which is one of the outages this signal exists to catch. Treating it as a
served generation would both hide the outage and, from an already-reported
outage, announce a recovery no client is getting. Every other 4xx is the
client's own malformed request and leaves generation health untouched — the same
exclusion §6.1 applies to credentials, and for the same reason: one misbehaving
project must not be able to mask an outage for everyone else.

Fields: consecutive failure count, last status.

This is the signal that survives whatever the cause: every provider credential
dead, a provider pool in cooldown, an upstream outage, or a credential killed
silently by the SDK's background refresh (§13). It observes the thing the
operator actually cares about — clients are not being served — rather than
inferring it from a credential population LLMGW cannot enumerate. Three
consecutive failures, rather than one, keeps an isolated upstream hiccup out of
the channel.

### 6.3 Project keys

Detected by a sweep on the existing worker goroutine: once immediately at
startup, then hourly. The immediate run means an expiry that fell during a
restart is reported at once rather than up to an hour later.

| Kind | Severity | Trigger |
|---|---|---|
| `project_key_expiring` | warning | not revoked, `expires_at` within the next 7 days |
| `project_key_expired` | warning | not revoked, `expires_at` in the past |

Fields: project name, key name, public ID, expiry timestamp.

Keys whose whole lifetime is 7 days or less (`expires_at - created_at ≤ 7d`) are
skipped: a deliberately short-lived key does not need a warning that it is
short-lived. Keys retired by `key rotate --overlap` keep their original creation
time, so a rotation with a long overlap will still produce an expiry warning for
the retired key — accepted, since that key really is about to stop working.

The sweep reads through one narrow port in `internal/domain/governance`,
consumed by the worker alone:

```go
// KeyExpiryReader reports keys whose expiry needs operator attention.
type KeyExpiryReader interface {
    // ExpiringKeys returns unrevoked keys expiring inside the window.
    ExpiringKeys(ctx context.Context, from, to time.Time) ([]KeyInfo, error)
}
```

It is deliberately *not* added to `governance.KeyRepository`: the only domain
consumer of that port, `projectkey.Service`, would gain a method it never calls,
and so would every test double. The query is read-only over existing columns —
no migration, and no index needed at this table's size.

The window is bounded on both sides: `from = now - 30 days`,
`to = now + 7 days`. Without the lower bound, a key that expired long ago and
was never revoked would be re-announced on every restart, forever, because dedup
state is in memory.

### 6.4 Budgets

Detected from `governance.Admission`, for **generation admissions only**.

| Kind | Severity | Trigger |
|---|---|---|
| `budget_blocked` | critical | first admission denied for a project/dimension/window/action |
| `budget_warning` | warning | first breach of a `warn` budget for that key |
| `budget_cleared` | info | a generation admission for that project no longer carries the breach |

Fields: project name, dimension, window, limit, reset time — except
`budget_cleared`, which carries project name, dimension, window and limit but no
reset time. Clearing is triggered by a breach's *absence*, so there is no
`BudgetBreach` left to read: the limit is carried over from the breach that was
notified, while a reset time for a window that has already rolled over would be
meaningless.

Clearing is defined as absence from `Blocks ∪ Warnings` of a generation
admission for that project. When one admission clears two budgets of the same
project, the two events have no defined order. Metadata requests carry an empty always-allowed
`Admission`, so treating them as evidence would post `budget_cleared` while
generations are still being denied.

`budget_cleared` is observed, not scheduled: it fires on the first admitted
generation after the rolling window moves on. A project that stops sending
traffic while blocked produces no clear event. That is accepted.

### 6.5 Gateway health

| Kind | Severity | Trigger |
|---|---|---|
| `gateway_started` | info | service constructed, about to run |
| `gateway_stopping` | info | `serve` returns, with the reason |
| `database_unavailable` | critical | first genuine PostgreSQL failure in the middleware or a worker |
| `database_restored` | info | first success after an unavailable state |
| `usage_correlation_poisoned` | critical | the terminal poisoned state that stops the service |

`gateway_started` marks startup, not readiness: the SDK opens its listener before
it can serve, and a `Run` that fails immediately will produce started then
stopping back to back — which is itself informative.

Database health is the one signal on the hot path, so it must cost nothing when
nothing is wrong: the tracker holds the down state in an atomic flag, and
`ObserveDatabase(true)` returns immediately without taking a lock while the flag
is clear. Success is reported from every repository call site that already
handles an error — admission, metadata recording, request completion, and the
reconciliation worker — so `database_restored` cannot be missed. Only a failure,
or the first success after one, reaches the transition engine.

### 6.6 Operator actions

Emitted by the local commands, not by the running gateway (§11).

| Kind | Severity | Trigger |
|---|---|---|
| `project_key_created` | info | `llmgw key create` |
| `project_key_rotated` | info | `llmgw key rotate` |
| `provider_credential_added` | info | `llmgw auth login`, whether or not the account was already authenticated |
| `provider_credentials_imported` | info | `llmgw auth import-legacy`, one summary event |

## 7. Configuration

```yaml
llmgw:
  discord-webhook-url-env: LLMGW_DISCORD_WEBHOOK_URL   # default
```

The YAML names the environment variable; it never carries the value. This
follows `postgres-dsn-env` and `key-pepper-env` exactly, and keeps the webhook
URL — a bearer secret, anyone holding it can post to the channel — out of the
config file and out of the repository.

`Config` exposes the resolution without retaining the value:

```go
// DiscordWebhookURL resolves the configured webhook without retaining it.
func (c Config) DiscordWebhookURL(getenv func(string) string) (string, bool, error)
```

Validation lives here and nowhere else; the adapter accepts what it is given.

- **Unset or empty → alerting disabled.** One log line at startup, no other
  effect anywhere. `.env.example` ships the variable empty for exactly this
  reason (§15).
- **Set → validated as an absolute `http` or `https` URL.** A malformed value
  fails the command that resolves it.
- **Set to a non-Discord host → accepted, with a startup warning.** The host is
  not part of the contract, and a self-hosted relay is a legitimate setup.

Resolution is lazy, exactly like `DatabaseDSN` and `KeyPepper`: only the
commands that notify resolve it. `serve`, `key create`, `key rotate`,
`auth login`, and `auth import-legacy` fail on a malformed URL; `key list`,
`key revoke`, `budget`, and `usage` are untouched. A typo in an optional
alerting variable must not block key revocation during an incident.

## 8. Discord Rendering

One embed per event, posted as JSON:

```json
{
  "username": "LLMGW",
  "embeds": [{
    "title": "Credential unauthorized",
    "description": "claude / clement@example.com stopped authenticating (HTTP 401)",
    "color": 14687012,
    "fields": [{"name": "Provider", "value": "claude", "inline": true}],
    "timestamp": "2026-07-30T12:00:00Z"
  }]
}
```

Colours: critical `0xE01B24`, warning `0xF5A623`, info `0x3BA55D`. Discord's
limits (title 256 chars, field value 1024, 25 fields) are enforced defensively
by truncation, so a long project name can never produce a rejected payload.

## 9. Delivery and Resilience

Discord being slow, rate-limiting, or down must never be visible to a client.

- **Emission never blocks.** `Notify` performs a non-blocking send on a buffered
  channel of 256 events and reports whether the event was accepted. A full queue
  drops the event and logs the drop with a running count — it never waits. The
  log is the observation point; this repository has no metrics surface and this
  feature does not add one.
- **One delivery goroutine** owns the HTTP client (10 s timeout), so ordering is
  preserved and concurrency against the webhook stays at one.
- **Bounded retry:** 3 attempts, 1 s then 2 s backoff, on transport errors and
  5xx. On 429, `Retry-After` is honoured, capped at 30 s. After the last
  attempt the event is dropped with a log line.
- **Throttle:** at least 1 s between two deliveries, below Discord's per-webhook
  rate limit.
- **`Close(ctx)` switches to drain mode:** it stops accepting new events and
  sends what is queued with one attempt each, no backoff and no throttle, under
  a 3 s per-event HTTP timeout, **newest first**, until the queue empties or the
  context expires. Newest-first matters: `gateway_stopping` is enqueued last, so
  a FIFO drain under a short budget would deliver stale events and lose the one
  the drain exists for. The steady-state retry policy cannot fit in a shutdown
  budget — one slow delivery alone exceeds it.

Callers give `Close` a 5 s budget: `serve` on the way out, each operator command
before it returns.

## 10. Wiring in `serve`

`runServeWith` owns the notifier and the tracker, because it is the only
function that spans the whole lifetime: it holds the context, the configuration
and `Getenv`, and its deferred block is where shutdown happens.

It resolves the webhook URL, builds the `discord.Webhook` and the `alert.Tracker`
(reading the credential label table via `ListAuth`), then hands the tracker to
the two `serveDependencies` seams that need it — `buildService` and
`startWorkers`, whose signatures gain the tracker, along with their stubs in
`serve_test.go`. `buildService` passes it to `NewMiddleware` and to the extra
`sdkusage.Plugin` slot of `NewService`, and wires it into the existing
`ReportPoisonWith` callback. `startWorkers` gains the tracker and the
`KeyExpiryReader` for the sweep.

`gateway_started` is emitted once the service is constructed; `gateway_stopping`
and `Close` run in the deferred block, after the lock release, so the last thing
the operator sees matches the process actually ending.

## 11. Operator Commands

`key create`, `key rotate`, `auth login`, and `auth import-legacy` already load
the configuration and read the environment, so each builds the webhook adapter
and a tracker with an empty label table, calls `Tracker.Emit`, and closes with a
5 s budget before returning. Going through the tracker rather than the notifier
keeps severity a property of the kind (§5.1).

These four commands therefore take up to five seconds longer to exit when
Discord is unreachable. A failed delivery never fails the command: the key was
created, the credential was added. The failure produces a warning on `stderr`
and nothing more.

Credentials dropped into `auth-dir` by hand are not notified. The SDK watcher is
deliberately disabled, so the gateway does not see them until it restarts.

## 12. Privacy and Security

Messages carry project names, key public IDs, provider names, credential labels,
model names, HTTP statuses, and timestamps. They never carry a project key, a
provider token, a request body, a model response, or any part of a prompt.

Two consequences are deliberate. Project names leave the machine — today the
gateway logs only numeric project IDs, so this is new. And for file-based OAuth
credentials the label is the account e-mail, which is what makes an alert
actionable: the operator needs to know which account to re-log. Those addresses
will appear in the Discord channel.

## 13. Accepted Limitations

- **A credential killed by the background refresh loop is not alerted as such.**
  In the pinned SDK, when the auto-refresh loop receives an unauthorized error
  from a token endpoint it marks the credential `Unavailable` in memory and
  reschedules, without executing a request and without writing cooldown state
  (`sdk/cliproxy/auth/conductor_refresh.go`). No usage record is produced, so
  nothing observable is emitted. Two consequences: `credential_unauthorized`
  fires only when a request actually reaches the dead credential, and the loss
  of capacity is caught by `generation_failures` (§6.2) once it starts costing
  clients. The alternative — supplying a `CooldownStateStore` through
  `WithCooldownStateStore` and turning on `save-cooldown-status` — was examined
  and does **not** close this gap, because that refresh path writes no cooldown
  state either; it would also mean implementing an SDK persistence port to use
  it as an observation channel. Reconsider only if a future SDK version emits on
  that path.
- **Rejected client credentials are not alerted.** The project-key domain
  returns one error for unknown, expired, and revoked keys alike, so a
  legitimate expired key is indistinguishable from a scan. The sweep covers
  expiry instead.
- **`budget_cleared` requires traffic.** See §6.4.
- **A queued alert is not a delivered alert.** The tracker records an entity as
  notified when the event enters the delivery queue, because a callback from the
  delivery goroutine would have to re-enter the tracker's lock. If Discord then
  refuses the event for all three attempts, or the shutdown drain runs out of
  budget, that transition is lost with only a log line behind it — and no later
  message corrects it while the entity's state holds. This narrows §5.2's "an
  alert can be late; it cannot be silently lost" to the queueing boundary:
  saturation and suppression are recoverable, a failed delivery is not. The case
  requires Discord to be unavailable across three attempts, in which case the
  alerts that follow are equally unlikely to arrive.
- **Dedup state is lost on restart.** Deliberate (§5.2). Expiry
  re-announcement is bounded by the 30-day window (§6.3); everything else is
  re-derived from live observation.

## 14. Testing

- **Domain unit tests** — the transition engine, table-driven: each state change
  emits once; repeats emit nothing; recovery fires only from a degraded state
  and only for the same credential *and* model; the anti-flap window holds per
  (entity, kind); a severity escalation passes it; a suppressed transition is
  re-emitted on the next observation; a rejected `Notify` leaves the delivered
  state untouched so the next observation retries; the key state never moves
  backwards; client 4xx statuses leave credential state untouched;
  `generation_failures` needs three consecutive 5xx or 429s, and a client 4xx
  neither clears a reported outage nor resets the count towards one.
- **Adapter unit tests, SDK side** — the observation contract of §5.1, which is
  not expressible in the domain because `ObserveDatabase` takes no context: a
  live-caller repository error marks the database down while a cancelled caller
  does not, and control records never reach `ObserveAttempt`.
- **Adapter unit tests** — against `httptest`: payload shape and colour per
  severity, `Retry-After` honoured on 429, retry exhaustion drops rather than
  blocks, a saturated queue drops and reports false, `Close` drains newest-first
  with one attempt per event, a disabled notifier performs no request.
- **Integration tests** — the real gateway against an in-process
  `alert.Notifier` sink. The gateway, the request path and the events stay real;
  only the HTTP hop is replaced, and it has its own exhaustive coverage above. A
  real webhook would impose the production one-second delivery throttle between
  every assertion, and nothing would own closing its server and goroutine at
  teardown while the harness audits the log for leaked secrets. The suite runs
  one service for the whole package, so the harness builds the tracker with a
  zero anti-flap window; the reachable events are the ones the request path
  produces: `credential_*` from a stub upstream forced to 429, `budget_blocked`
  from a breached `calls` budget, `generation_failures` from repeated upstream
  5xx, and the absence of a second message for a repeated identical failure. Startup, shutdown, sweep and
  operator events are not reachable through that harness and are covered by unit
  tests of their own call sites instead — stated here rather than promised and
  quietly dropped. Assertions are on shape and plausibility — kind, severity,
  presence of the entity — never on exact wording.
- **Configuration tests** — malformed URL rejected by the commands that resolve
  it and ignored by those that do not, absent variable disables cleanly, a
  non-Discord host is accepted with a warning.

## 15. Deployment Notes

- New environment variable `LLMGW_DISCORD_WEBHOOK_URL`, optional. Absent or
  empty means the feature is off.
- `.env.example` gains `LLMGW_DISCORD_WEBHOOK_URL=` — **empty, not a
  `CHANGE_ME` placeholder**: the README tells operators to copy that file
  verbatim, and `CHANGE_ME` is not an absolute URL, so it would fail `serve`.
- `config.example.yaml` gains the `discord-webhook-url-env` key; `README.md`
  gains the variable in its deployment-inputs list.
- No migration, no volume change, no port change, no compose change
  (`env_file: .env` already covers it).
- Outbound HTTPS to `discord.com` must be reachable from the container.
