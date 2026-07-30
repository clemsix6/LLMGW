# Discord Alerting Implementation Plan

> **For agentic workers:** this plan is executed by the project pipeline — one
> `pipeline-batch-manager` per batch, in order, one at a time. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver one outbound Discord webhook that posts a message when a
provider credential, a generation path, a project key, a budget, or the gateway
itself changes state.

**Architecture:** A pure transition engine in
`internal/domain/governance/alert` turns observed facts into events; a
`internal/adapter/discord` webhook delivers them without ever blocking a
request; `internal/command` wires the two and feeds them from the middleware, an
extra SDK usage plugin, and the background workers.

**Tech Stack:** Go 1.26 (per `go.mod`), embedded CLIProxyAPI v7.2.102 SDK,
PostgreSQL, `net/http`, `testing` + `httptest`, testcontainers for the
integration suite.

## As-built after Batch 1 — read before implementing anything else

Batch 1 landed at `f82ecb6`. The engine differs from this plan's pseudo-code in
ways later batches build on:

- The primitive is
  `transitionLocked(key, state string, healthy bool, kind Kind, summary string, fields []Field)`,
  split into `shouldEmitLocked` / `escalates` / `commitLocked`. `entry` carries
  `deliveredKind` and `deliveredHealthy` beyond the four fields shown below.
- **Every observer holds the tracker mutex across `Notify`.** Therefore
  `Webhook.Notify` must never block and must never call back into the tracker.
  This is a hard constraint on Batch 2, not a preference.
- The window is bypassed only when the incoming kind outranks the last delivered
  kind **and** the delivered state was degraded — a never-delivered kind already
  bypasses it, so an unconditional severity comparison would let a credential
  flapping 429/ok burst through.
- `ObserveAttempt` zeroes the status when `!failed`, so `credential_recovered`
  renders no status field, and it returns early on an empty credential ID.
- `ObserveGeneration` keeps its counter and last status on `Tracker`; below
  three consecutive failures it does not transition at all.
  `generation_recovered` carries `Consecutive failures: 0`.
- `ObserveProjectKeys` also skips an expiry further than 7 days away, which the
  plan's bare "otherwise → expiring" did not. The lifetime skip boundary is
  `<=`: exactly 7 days is skipped.
- Database events carry no fields. `New` defaults a nil `now` to `time.Now`.
- `budget_cleared` carries project, dimension, window **and limit** — the limit
  is carried over from the notified breach. Spec §6.4 was amended to match. When
  one admission clears two budgets of the same project, the events have **no
  defined order**: Task 6.2 must not assert one.
- The kind title had an unexported accessor `title()` with nine call sites.
  Task 2.1 **promoted it to `Title()`** rather than adding a second wrapper, so
  the rename touched every file in the `alert` package. `title()` no longer
  exists: any later code written against it will not compile.

## As-built after Batch 2 — the adapter

Batch 2 landed at `54bf9b5`. What later batches must know:

- **`Close`'s error contract**, which Batch 5 logs in its deferred block: `nil`
  on a nil receiver, on a second call, and on a complete drain — individual
  delivery failures are logged, not returned. A **wrapped `ctx.Err()`** means
  only that the drain was cut short. Batch 5 must not treat a non-nil `Close`
  error as a startup-class failure, and must not let it change the process exit
  status.
- On an early `Close` return the delivery goroutine outlives `Close` by design.
  There is no signal that the socket is free; fine at shutdown, but do not
  assume otherwise.
- The renderer **omits `description` when it equals the title**, and Batch 1
  sets `Summary` to the kind title for nearly every event. **Batch 6 must not
  assert a `description` on those events.**
- Fields with an empty name or value are skipped: Discord answers 400 on an
  empty field value, and a 400 is permanent, so the whole alert would vanish.
- `Retry-After` is parsed as a float — Discord sends fractional seconds.
- The per-attempt bound is a `context.WithTimeout` per request; `client.Timeout`
  is only the nil-client fallback. `newWithTimings` tolerates a nil client and a
  nil clock.
- **`Notify` returning `true` means queued, not delivered.** See the accepted
  limitation added to spec §13.

## As-built after Batch 3 — configuration, expiry query, worker port

Batch 3 landed at `c6806c8`. What later batches must know:

- **`ExpiringKeys` projects `created_at`.** The plan described the query without
  it, which would have silently disabled spec §6.3's short-lifetime skip
  forever: `alert/keys.go` computes the lifetime as
  `ExpiresAt.Sub(CreatedAt)`, and a zero `CreatedAt` makes every key look two
  thousand years old. Nothing downstream would have caught it — Batch 5 tests
  the sweep against a stub and Batch 6 declares sweep events unreachable.
- `(*Store).ExpiringKeys` has its own `scanKeyInfo`; `scanClientKey` is
  untouched. New package-level test helpers now exist in
  `internal/adapter/postgres`: `seedExpiryKey`, `ptrTime`, `expiryProject`,
  `assertPublicIDs`, `assertKeyProjected`. **A later task adding its own
  `ptrTime` there will collide.**
- `internal/config/discord.go` exports only `Config.DiscordWebhookURL`;
  validation is the unexported `validateWebhookURL`, the constant is
  `defaultDiscordWebhookURLEnv`. The resolver trims its value before the empty
  test, and short-circuits when the env *name* is empty — `applyLLMGWDefaults`
  runs only through `Load`, so hand-built `config.Config` values such as
  `validServeConfig()` never receive the default.
- **`workers.go` carries no `var _ workerRepository = (*postgres.Store)(nil)`
  assertion** and does not import the adapter — the guarantee comes from the
  production call site in `serve.go`. Batch 5 must not expect one, and must not
  reintroduce the import: it would undo the narrow port's purpose.
- `StartWorkers` is still 3 parameters, and `internal/command/workers_test.go`
  now exists with a double calling it at 3. Batch 5 adds the fourth and must
  update that file — it will break visibly at compile time.

**Process note for any future batch: `parallel` is unsafe when tasks are
red-first under a whole-tree gate.** Batch 3's two parallel tasks each run
`go vet ./...`, `go build ./...` and `go test ./internal/...`, so each would
have seen the other's deliberately-failing tree with no way to tell it from its
own failure. They were run sequentially.

**Spec:** `docs/superpowers/specs/2026-07-30-discord-alerting-design.md`
(approved at commit `851f9b0`). Where this plan and the spec disagree, the spec
wins — report the divergence rather than silently following one.

## Global Constraints

- Go standards: functions 15-25 lines (30 is already too long), files 200-300
  lines (400 is a hard smell), docstring on every exported symbol and every
  struct field, errors wrapped as `fmt.Errorf("context:\n%w", err)`.
- Minimal public API: export only what another package actually calls.
- The CLIProxyAPI SDK is consumed unmodified. No fork, no patch, no `replace`
  directive in `go.mod`. This feature adds no third-party dependency: `net/http`
  and the standard library only.
- No new inbound surface. This feature only makes outbound HTTPS calls.
- No secret in the repository: the webhook URL lives in an environment variable
  only. Never commit a real webhook URL, not even in a test fixture.
- Alerting is optional and must never be able to fail the gateway: a nil
  `*alert.Tracker` receiver, a nil notifier, an unreachable Discord, and a
  failed credential-label lookup all degrade silently.
- 1 task = 1 commit. Commit messages follow the repository convention: a title
  line with no prefix, a blank line, then `[+]` / `[&]` / `[!]` / `[-]` entries.
  No footers, no co-author trailers.

**Gates.** The default gate for every task is
`just fmt && just vet && just build && just test-unit`. **Exactly one task
reduces it: Task 4.1**, which changes a signature whose out-of-package callers
belong to its batch's integration task. Note that `just vet` is `go vet ./...`,
which type-checks the whole tree including tests — a reduced gate must narrow
`vet` and `build` to the package under work, not merely skip `test-unit`. Every
other task passes the full gate as written. **Every batch needs Docker**, not
only those touching PostgreSQL: `just test-unit` runs `./internal/...`, which
includes the testcontainers packages, and a missing daemon makes them skip
rather than fail — a skipped test is not a pass.

## Frozen Interfaces

Every task depends on these names. They are decided here so parallel tasks
cannot disagree; changing one is an as-built divergence to report.

```go
// package alert — internal/domain/governance/alert

type Kind string

const (
    KindCredentialUnauthorized Kind = "credential_unauthorized"
    KindCredentialRateLimited  Kind = "credential_rate_limited"
    KindCredentialFailing      Kind = "credential_failing"
    KindCredentialRecovered    Kind = "credential_recovered"
    KindGenerationFailures     Kind = "generation_failures"
    KindGenerationRecovered    Kind = "generation_recovered"
    KindProjectKeyExpiring     Kind = "project_key_expiring"
    KindProjectKeyExpired      Kind = "project_key_expired"
    KindBudgetBlocked          Kind = "budget_blocked"
    KindBudgetWarning          Kind = "budget_warning"
    KindBudgetCleared          Kind = "budget_cleared"
    KindGatewayStarted         Kind = "gateway_started"
    KindGatewayStopping        Kind = "gateway_stopping"
    KindDatabaseUnavailable    Kind = "database_unavailable"
    KindDatabaseRestored       Kind = "database_restored"
    KindUsagePoisoned          Kind = "usage_correlation_poisoned"
    KindProjectKeyCreated      Kind = "project_key_created"
    KindProjectKeyRotated      Kind = "project_key_rotated"
    KindCredentialAdded        Kind = "provider_credential_added"
    KindCredentialsImported    Kind = "provider_credentials_imported"
)

type Severity string

const (
    SeverityCritical Severity = "critical"
    SeverityWarning  Severity = "warning"
    SeverityInfo     Severity = "info"
)

// DefaultWindow is the minimum re-notification interval per entity and kind.
const DefaultWindow = 15 * time.Minute

type Field struct {
    Name  string
    Value string
}

type Event struct {
    Kind     Kind
    Severity Severity
    Summary  string
    Fields   []Field
    At       time.Time
}

// Notify is called with the tracker's mutex held: it must never block and must
// never call back into the tracker.
type Notifier interface {
    Notify(Event) bool
}

// Title is the human title of a kind, shared by Emit summaries and the Discord
// renderer. Added by Task 2.1.
func (k Kind) Title() string

// CredentialLabel is the operator-facing identity of one provider credential.
type CredentialLabel struct {
    Provider string
    Label    string
}

func New(
    notifier Notifier,
    labels map[string]CredentialLabel,
    window time.Duration,
    now func() time.Time,
) *Tracker

func (t *Tracker) ObserveAttempt(provider, credentialID, model string, failed bool, status int)
func (t *Tracker) ObserveGeneration(status int)
func (t *Tracker) ObserveAdmission(project string, blocks, warnings []governance.BudgetBreach)
func (t *Tracker) ObserveProjectKeys(keys []governance.KeyInfo, now time.Time)
func (t *Tracker) ObserveDatabase(healthy bool)
func (t *Tracker) Emit(kind Kind, fields ...Field)
```

Every `Tracker` method is safe on a nil receiver and on a nil notifier: it
returns immediately. That is what makes wiring nil-tolerant everywhere else.

```go
// package discord — internal/adapter/discord

// New builds a webhook with the production timings.
func New(webhookURL string, client *http.Client, now func() time.Time) *Webhook

func (w *Webhook) Notify(event alert.Event) bool

// Close is safe to call twice and safe on a nil receiver.
func (w *Webhook) Close(ctx context.Context) error

// package governance — internal/domain/governance/ports.go

type KeyExpiryReader interface {
    ExpiringKeys(context.Context, time.Time, time.Time) ([]KeyInfo, error)
}

// package config — internal/config

func (c Config) DiscordWebhookURL(getenv func(string) string) (string, bool, error)

// package cliproxy — internal/adapter/cliproxy

func NewAlertUsagePlugin(tracker *alert.Tracker) sdkusage.Plugin

func NewMiddleware(
    keys KeyAuthenticator,
    requests governance.RequestRepository,
    now func() time.Time,
    bridge *UsageBridge,
    tracker *alert.Tracker, // nil disables observation
) *Middleware

// package command — internal/command

// workerRepository embeds the expiry port; StartWorkers gains the tracker.
type workerRepository interface {
    governance.KeyExpiryReader

    ReconcileAccounting(context.Context, time.Time, time.Duration, time.Duration) (governance.ReconcileResult, error)
    PruneCompletedRequests(context.Context, time.Duration) (int64, error)
}

// operatorNotifier delivers one operator event from a short-lived command.
//
// Construction resolves the webhook URL, so a command can fail before doing its
// work; emit never fails the command.
type operatorNotifier struct{ /* unexported */ }

func newOperatorNotifier(cfg config.Config, streams Streams) (*operatorNotifier, error)
func (n *operatorNotifier) emit(kind alert.Kind, fields ...alert.Field)

func StartWorkers(
    ctx context.Context,
    repo workerRepository,
    retention time.Duration,
    tracker *alert.Tracker,
) <-chan struct{}

// serveDependencies field signatures both gain the tracker:
//   buildService func(config.Config, *serveStore, []byte, *alert.Tracker) (serveService, error)
//   startWorkers func(context.Context, *serveStore, time.Duration, *alert.Tracker) <-chan struct{}
```

## File Structure

| File | Responsibility |
|---|---|
| `internal/domain/governance/alert/event.go` | `Kind`, `Severity`, `Field`, `Event`, `Notifier`, kind→severity and kind→title tables |
| `internal/domain/governance/alert/tracker.go` | `Tracker`, `New`, `Emit`, the shared `transition` primitive |
| `internal/domain/governance/alert/credentials.go` | `ObserveAttempt` and credential state |
| `internal/domain/governance/alert/health.go` | `ObserveGeneration`, `ObserveDatabase` |
| `internal/domain/governance/alert/budgets.go` | `ObserveAdmission` |
| `internal/domain/governance/alert/keys.go` | `ObserveProjectKeys` |
| `internal/adapter/discord/render.go` | Event → Discord embed payload, colours, truncation |
| `internal/adapter/discord/webhook.go` | Queue, delivery goroutine, `Notify`, `Close` |
| `internal/adapter/discord/delivery.go` | One HTTP delivery, retry policy, `Retry-After` |
| `internal/config/discord.go` | Webhook URL resolution and validation (keeps `config.go` from growing) |
| `internal/adapter/cliproxy/alert_plugin.go` | `alertUsagePlugin`: SDK usage record → `ObserveAttempt` |
| `internal/command/alerting.go` | Notifier/tracker construction and `notifyOperatorEvent` |
| `test/integration/alerts_test.go` | Alert coverage against a stub webhook |

Modified: `internal/config/config.go` (field + default only),
`internal/domain/governance/ports.go`, `internal/adapter/postgres/keys.go`,
`internal/adapter/cliproxy/middleware.go`,
`internal/command/{serve,workers,key,auth}.go`,
`test/integration/harness_test.go`, `.env.example`, `config.example.yaml`,
`README.md`.

---

# Batch 1 — Transition engine

Foundation consumed by every later batch. No wiring yet, so the batch ends with
the tests that exercise it.

### Task 1.1: Alert types and the transition engine — `same-agent`, **model: opus**

**Files:**
- Create: `internal/domain/governance/alert/event.go`
- Create: `internal/domain/governance/alert/tracker.go`
- Create: `internal/domain/governance/alert/credentials.go`
- Create: `internal/domain/governance/alert/health.go`
- Create: `internal/domain/governance/alert/budgets.go`
- Create: `internal/domain/governance/alert/keys.go`
- Test: `internal/domain/governance/alert/tracker_test.go` (one smoke test only;
  Task 1.2 owns the exhaustive suite)

**Interfaces:**
- Consumes: `governance.BudgetBreach`, `governance.BudgetLimit`,
  `governance.KeyInfo` from the parent package.
- Produces: everything under `package alert` in "Frozen Interfaces".

**Field sets per kind — read spec §6.1 to §6.6 and implement exactly those:**
credentials carry provider, credential label (or ID when the label map has no
entry), model, and upstream status (omitted entirely when the status is ≤ 0);
generation health carries the consecutive failure count and the last status;
project keys carry project name, key name, public ID, expiry; budgets carry
project name, dimension, window, limit, reset time — except `budget_cleared`,
which carries only project, dimension and window, since no breach remains to
read a limit from. A statusless transport failure says so in its summary rather
than rendering a status of zero. No field may ever carry a key, a token, a
request body, or a model response.

**Design notes the implementer must honour:**

One map from entity key to entry, under one mutex, plus an `atomic.Bool` for
database-down so the healthy hot path takes no lock.

```go
// entry is the tracked state of one entity.
type entry struct {
    current   string             // current is the state last observed.
    delivered string             // delivered is the state last accepted by the notifier.
    at        map[Kind]time.Time // at is when each kind was last delivered for this entity.
    fields    []Field            // fields are the identifying fields of the last breach, for clearing events.
}
```

`at` is keyed by kind because spec §5.2 scopes the window per **(entity, kind)**:
a different transition on the same entity is never suppressed by the previous
one. `fields` exists because `budget_cleared` is triggered by a breach's
*absence*, so there is no `BudgetBreach` left to read at clear time — the clear
event carries only the identifying project, dimension and window, which is what
Task 1.2 and Task 6.2 must both assert.

Entity keys are strings built by the observation methods:
`"credential\x00" + credentialID + "\x00" + model`, `"generation"`,
`"budget\x00" + project + "\x00" + dimension + "\x00" + window + "\x00" + action`,
`"key\x00" + publicID`, `"database"`.

The single transition primitive every observer calls:

```go
// transition emits when the observed state differs from what was delivered,
// deferring rather than discarding when the anti-flap window is still open.
func (t *Tracker) transition(key, state string, kind Kind, summary string, fields []Field) {
    // 1. Set entry.current = state, always.
    // 2. Return if state == entry.delivered.
    // 3. Return if entry.delivered == "" and state is the healthy state — an
    //    entity first seen healthy has not transitioned. This is what keeps
    //    credential_recovered, generation_recovered and database_restored from
    //    firing on a first observation.
    // 4. Return if now-entry.at[kind] < window AND kind's severity is not
    //    higher than the severity of the kind last delivered for this entity —
    //    without touching delivered, so a later observation retries.
    // 5. Build the Event and call notifier.Notify.
    // 6. Only if Notify returned true, set entry.delivered = state, entry.at =
    //    now, and remember the delivered kind for step 4.
}
```

Steps 4 and 6 are the whole point: a suppressed or dropped event leaves
`delivered` behind `current`, so the next observation of that entity re-emits.

Per-observer rules:

- `ObserveAttempt`: status 401 or 403 → `unauthorized`; 429 → `rate_limited`;
  `failed` with status ≥ 500 or status ≤ 0 → `failing`; `failed` with any other
  4xx → **return without touching state** (a client's malformed request is not
  a credential problem); not failed → `ok`.
- `ObserveGeneration`: status ≥ 500 increments a consecutive counter; at three
  or more the state is `failing`. Status < 500 resets the counter and sets `ok`.
- `ObserveAdmission`: each breach in `blocks` → `blocked`, each in `warnings` →
  `warned`; every tracked budget entity of that project present in neither →
  `ok`.
- `ObserveProjectKeys`: `expires_at` in the past → `expired`; otherwise
  `expiring`; state never moves backwards, so an entity already `expired`
  ignores `expiring`. Skip any key whose whole lifetime
  (`ExpiresAt.Sub(CreatedAt)`) is ≤ 7 days — a deliberately short-lived key
  needs no warning that it is short-lived.
- `ObserveDatabase(false)`: set the atomic flag, transition to `down`.
  `ObserveDatabase(true)`: return immediately while the flag is clear;
  otherwise clear it and transition to `up`.
- `Emit`: one-shot, no state, always notifies. Its summary comes from the
  kind→title table in `event.go`.

- [ ] **Step 1: Write the smoke test**

```go
func TestTrackerEmitsOncePerTransition(t *testing.T) {
    sink := &recordingNotifier{}
    tracker := alert.New(sink, nil, alert.DefaultWindow, func() time.Time { return fixedNow })

    tracker.ObserveAttempt("claude", "cred-1", "claude-opus-5", true, 429)
    tracker.ObserveAttempt("claude", "cred-1", "claude-opus-5", true, 429)

    if len(sink.events) != 1 {
        t.Fatalf("events = %d, want 1", len(sink.events))
    }
    if sink.events[0].Kind != alert.KindCredentialRateLimited {
        t.Fatalf("kind = %q, want %q", sink.events[0].Kind, alert.KindCredentialRateLimited)
    }
    if sink.events[0].Severity != alert.SeverityWarning {
        t.Fatalf("severity = %q, want %q", sink.events[0].Severity, alert.SeverityWarning)
    }
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/domain/governance/alert/ -run TestTrackerEmitsOncePerTransition -v`
Expected: build failure — the package does not exist yet.

- [ ] **Step 3: Write `event.go`**

The constants from "Frozen Interfaces", plus two tables:

```go
// severityOf maps every kind to its fixed severity.
var severityOf = map[Kind]Severity{
    KindCredentialUnauthorized: SeverityCritical,
    KindGenerationFailures:     SeverityCritical,
    KindBudgetBlocked:          SeverityCritical,
    KindDatabaseUnavailable:    SeverityCritical,
    KindUsagePoisoned:          SeverityCritical,

    KindCredentialRateLimited: SeverityWarning,
    KindCredentialFailing:     SeverityWarning,
    KindProjectKeyExpiring:    SeverityWarning,
    KindProjectKeyExpired:     SeverityWarning,
    KindBudgetWarning:         SeverityWarning,

    // every remaining kind is SeverityInfo
}

// titleOf maps every kind to its human title, also used as the Emit summary.
var titleOf = map[Kind]string{ /* one entry per kind, e.g. "Gateway started" */ }
```

`func (k Kind) severity() Severity` returns `SeverityInfo` for anything absent
from the table, so a new kind can never panic.

- [ ] **Step 4: Write `tracker.go`, then the four observer files**

Keep every observer under 25 lines by giving each its own classification
helper, e.g. `credentialState(failed bool, status int) (state string, observe bool)`.

- [ ] **Step 5: Run the smoke test**

Run: `go test -race ./internal/domain/governance/alert/ -v`
Expected: PASS.

- [ ] **Step 6: Gates and commit**

```bash
just fmt && just vet && just build && just test-unit
git add internal/domain/governance/alert
git commit -F - <<'EOF'
Alert transition engine

[+] alert package: Kind, Severity, Field, Event, Notifier
[+] Tracker with per-entity state, anti-flap window and deferred suppression
[+] Credential, generation, budget, project-key and database observers
EOF
```

### Task 1.2 (integration): Exhaustive engine tests — dispatched alone, **model: opus**

**Files:**
- Test: `internal/domain/governance/alert/tracker_test.go`
- Test: `internal/domain/governance/alert/credentials_test.go`
- Test: `internal/domain/governance/alert/budgets_test.go`
- Test: `internal/domain/governance/alert/keys_test.go`

**Interfaces:**
- Consumes: the whole `alert` public surface from Task 1.1.
- Produces: nothing consumed downstream.

Every case below is required — this suite is the only place the engine's
behaviour is pinned.

- [ ] **Step 1: Credential cases**

A repeat emits nothing. The same credential on a different model is a separate
entity and emits its own event. `credential_recovered` fires only after a
delivered degraded state, and only for the same (credential, model) pair — a
first-ever success emits nothing. A failed attempt with status 400, 404 or 422
leaves the state untouched and emits nothing. A failed attempt with status 0
emits `credential_failing` and carries no status field. A credential present in
the label map renders its label; one absent renders its ID.

- [ ] **Step 2: Anti-flap and suppression cases**

Inside the window, a same-severity change emits nothing, and a later observation
after the window re-emits the still-current state. An escalation from warning to
critical emits immediately, inside the window. A notifier returning false leaves
`delivered` behind, so the next identical observation emits again.

- [ ] **Step 3: Generation, budget, key and database cases**

Two 5xx emit nothing and the third emits `generation_failures`; a success resets
the counter so three more are needed. A `warn` and a `block` budget on the same
dimension and window are separate entities and can both be active. Each emits
its own `budget_cleared` only from a delivered breach. A key already `expired`
never re-emits `expiring`; a key whose lifetime is ≤ 7 days emits nothing.
`ObserveDatabase(true)` on a healthy tracker emits nothing; after a failure it
emits `database_restored` exactly once.

- [ ] **Step 4: Nil-safety and privacy**

A nil `*Tracker` and a tracker built with a nil notifier accept every
observation without panicking and emit nothing. Assert that no event's fields
contain a value the test seeded as a secret.

- [ ] **Step 5: Run with the race detector**

Run: `go test -race -count=1 ./internal/domain/governance/alert/ -v`
Expected: PASS. Include a test driving `ObserveAttempt` and `ObserveGeneration`
from 50 goroutines to prove the mutex holds.

- [ ] **Step 6: Gates and commit**

```bash
just fmt && just vet && just build && just test-unit
git add internal/domain/governance/alert
git commit -F - <<'EOF'
Alert engine test suite

[+] Credential, generation, budget, key and database transition cases
[+] Anti-flap, escalation and deferred-suppression cases
[+] Nil-safety, privacy and concurrent observation cases
EOF
```

---

# Batch 2 — Discord adapter

### Task 2.1: Embed rendering — `same-agent`, **model: opus**

**Files:**
- Create: `internal/adapter/discord/render.go`
- Test: `internal/adapter/discord/render_test.go`
- Modify: `internal/domain/governance/alert/event.go` — add
  `func (k Kind) Title() string` over the existing unexported `titleOf` table,
  with its docstring, and one test asserting a known kind and an unknown one.
  This is the only domain change this batch may make; it exists because the
  renderer needs the title and a second copy of it would drift.

**Interfaces:**
- Consumes: `alert.Event`, `alert.Severity`, `alert.Kind.Title`.
- Produces: `func renderPayload(event alert.Event) ([]byte, error)` — unexported;
  Task 2.2 calls it from the same package.

- [ ] **Step 1: Write the failing test**

Assert the marshalled payload has `username` `"LLMGW"`, exactly one embed, the
event's summary as `description`, its kind's title as `title`, an RFC 3339
`timestamp`, every field rendered with `"inline": true`, and the colour per
severity: critical **14687012** (`0xE01B24`),
warning **16098851** (`0xF5A623`), info **3908957** (`0x3BA55D`). Assert a
2000-character field value is truncated to 1024, a 300-character title to 256,
and a 30-field event to 25 fields.

- [ ] **Step 2: Run it and watch it fail** — `go test ./internal/adapter/discord/ -v`

- [ ] **Step 3: Implement `render.go`** — private payload structs with JSON
  tags, one colour table keyed by severity, one
  `truncate(value string, limit int) string` helper.

- [ ] **Step 4: Run the tests** — Expected: PASS.

- [ ] **Step 5: Gates and commit**

```bash
just fmt && just vet && just build && just test-unit
git add internal/adapter/discord
git commit -F - <<'EOF'
Discord embed rendering

[+] Event to Discord embed payload with severity colours
[+] Defensive truncation of title, field values and field count
EOF
```

### Task 2.2: Queue, delivery and drain — `same-agent`, **model: opus**

**Files:**
- Create: `internal/adapter/discord/webhook.go`
- Create: `internal/adapter/discord/delivery.go`
- Test: `internal/adapter/discord/webhook_test.go`

**Interfaces:**
- Consumes: `renderPayload` from Task 2.1, `alert.Event`.
- Produces: `New`, `(*Webhook).Notify`, `(*Webhook).Close`, **and the
  unexported test constructor Task 2.3 depends on:**

```go
// timings groups the delays so tests can shrink them.
type timings struct {
    backoffs    []time.Duration // backoffs is the retry schedule.
    throttle    time.Duration   // throttle is the minimum gap between deliveries.
    attempt     time.Duration   // attempt is the per-request timeout.
    drainAttempt time.Duration  // drainAttempt is the per-request timeout while draining.
    maxRetryAfter time.Duration // maxRetryAfter caps an honoured Retry-After.
}

// newWithTimings builds a webhook with test-controlled delays.
func newWithTimings(webhookURL string, client *http.Client, now func() time.Time, t timings) *Webhook
```

`New` calls `newWithTimings` with the production values: backoffs `1s, 2s`,
throttle `1s`, attempt timeout `10s` (also the default `http.Client` timeout
when the caller passes nil), drain attempt `3s`, max `Retry-After` `30s`.

**Design notes:**

- Buffered channel of 256. `Notify` uses `select` with `default` — never blocks.
  It returns false when the queue is full or after `Close`, and logs the drop
  with a running count. **The tracker calls `Notify` while holding its mutex**
  (see "As-built after Batch 1"), so `Notify` must not block, must not perform
  network I/O, and must never call back into the tracker. Everything slow
  belongs to the delivery goroutine. The drop log that spec §9 requires is the
  one exception and stays.
- One goroutine consumes the channel. Steady state: up to 3 attempts, backoff
  between them, on transport errors and 5xx. A 4xx other than 429 is permanent —
  no retry. On 429, honour `Retry-After` capped at `maxRetryAfter`. At least
  `throttle` between two deliveries.
- `Close(ctx)` stops accepting, then drains **newest first**, one attempt each,
  no backoff, no throttle, `drainAttempt` timeout, until the queue empties or
  `ctx` expires. Newest-first is deliberate: `gateway_stopping` is enqueued last
  and is the event the drain exists for. `Close` is idempotent and safe on a nil
  receiver.

  A channel cannot be read newest-first, so the ownership handover must be
  explicit and is the one place a race can hide: `Close` flips the accepting
  flag under the same mutex `Notify` uses, closes the channel, waits on the
  consumer's `done` channel so exactly one goroutine ever touches the socket,
  then reads what remains of the buffer into a slice, reverses it, and delivers.
  A second `Close` observes the flag and returns nil without repeating any of
  it.

- [ ] **Step 1: Write the failing tests against `httptest`**

A delivered event produces exactly one POST whose body matches the rendered
payload. A 429 carrying `Retry-After: 0` is retried once then succeeds. Three
consecutive 500s produce three attempts then a drop. A 400 produces exactly one
attempt. A saturated queue makes `Notify` return false without blocking. `Close`
delivers the most recently queued event first. `Close` on an already-closed
webhook returns without panicking.

- [ ] **Step 2: Run and watch them fail** — `go test ./internal/adapter/discord/ -v`

- [ ] **Step 3: Implement `webhook.go` and `delivery.go`** — split so neither
  passes 300 lines: `webhook.go` owns lifecycle and the queue, `delivery.go`
  owns one attempt plus the retry policy.

- [ ] **Step 4: Run with the race detector**

Run: `go test -race -count=1 ./internal/adapter/discord/ -v`
Expected: PASS, package under 10 seconds — tests use `newWithTimings` with
sub-millisecond delays, never the production schedule.

- [ ] **Step 5: Gates and commit**

```bash
just fmt && just vet && just build && just test-unit
git add internal/adapter/discord
git commit -F - <<'EOF'
Discord webhook delivery

[+] Bounded queue with non-blocking Notify reporting acceptance
[+] Single delivery goroutine with bounded retry and Retry-After handling
[+] Newest-first drain on Close so shutdown events survive a slow Discord
EOF
```

### Task 2.3 (integration): Adapter behaviour under failure — dispatched alone, **model: opus**

**Files:**
- Test: `internal/adapter/discord/webhook_failure_test.go`

**Interfaces:**
- Consumes: `newWithTimings` from Task 2.2 — use it, never `New`, so the
  package stays fast.

- [ ] **Step 1: Add the failure cases**

A server that never answers must not block `Notify`, and `Close` must return
when its context expires. A `Close` given an already-expired context returns
immediately and drops what remains. A webhook whose `Close` ran still accepts
`Notify` calls without panicking, returning false.

- [ ] **Step 2: Run with the race detector**

Run: `go test -race -count=1 ./internal/adapter/discord/ -v`
Expected: PASS, whole package under 10 seconds.

- [ ] **Step 3: Gates and commit**

```bash
just fmt && just vet && just build && just test-unit
git add internal/adapter/discord
git commit -F - <<'EOF'
Discord adapter failure coverage

[+] Unresponsive server, expired-context drain and post-close Notify cases
EOF
```

---

# Batch 3 — Configuration, expiry query, worker port

Tasks 3.1 and 3.2 touch disjoint packages and compile independently. Task 3.3
is the integration task: it makes `ExpiringKeys` reachable from the worker
seam, which is what Batch 5 then schedules.

### Task 3.1: Webhook configuration — `parallel`

**Files:**
- Create: `internal/config/discord.go`
- Modify: `internal/config/config.go` (the `LLMGW` field and its default only —
  the file is already 516 lines, keep the new logic in `discord.go`)
- Test: `internal/config/discord_test.go`

**Interfaces:**
- Produces: `LLMGW.DiscordWebhookURLEnv` (yaml `discord-webhook-url-env`,
  default `LLMGW_DISCORD_WEBHOOK_URL`) and `Config.DiscordWebhookURL`.

**Behaviour:** empty or unset → `("", false, nil)`. Parseable as an absolute
`http`/`https` URL with a host → `(url, true, nil)`. Anything else → an error
wrapped like the neighbouring resolvers. The resolver does **not** judge the
host; the caller warns when it is not `discord.com` or `discordapp.com`.

- [ ] **Step 1: Write the failing tests** — unset, empty, a Discord URL, a
  non-Discord https URL (accepted), `not-a-url`, `ftp://host/x`,
  `https:///nohost`.
- [ ] **Step 2: Run and watch them fail** — `go test ./internal/config/ -v`
- [ ] **Step 3: Add the field, its default in `applyLLMGWDefaults`, and
  `discord.go`**
- [ ] **Step 4: Run the tests** — Expected: PASS.
- [ ] **Step 5: Gates and commit**

```bash
just fmt && just vet && just build && just test-unit
git add internal/config
git commit -F - <<'EOF'
Discord webhook configuration

[+] llmgw.discord-webhook-url-env with its default
[+] Config.DiscordWebhookURL resolver validating absolute http(s) URLs
EOF
```

### Task 3.2: Key expiry port and query — `parallel`, **model: opus**

**Files:**
- Modify: `internal/domain/governance/ports.go`
- Modify: `internal/adapter/postgres/keys.go`
- Test: `internal/adapter/postgres/keys_expiry_test.go`

**Interfaces:**
- Produces: `governance.KeyExpiryReader` and
  `func (s *Store) ExpiringKeys(ctx context.Context, from, to time.Time) ([]governance.KeyInfo, error)`.

**Behaviour:** unrevoked keys with a non-null `expires_at` inside `[from, to]`,
joined to their project for the name, ordered by `expires_at` ascending.
Read-only over the existing schema — no migration, no index.

Two notes for the implementer. The existing `scanClientKey`
(`internal/adapter/postgres/keys.go`) returns `governance.ClientKey`, which
carries `Digest`; this query returns `governance.KeyInfo`, which does not, so it
needs its own projection and scan — do not widen the existing one. And follow
the package convention by adding
`var _ governance.KeyExpiryReader = (*Store)(nil)` next to the neighbouring
assertions, so the port has a compile-time consumer.

- [ ] **Step 1: Write the failing test** — seed one project with four keys: one
  expiring in 3 days (returned), one expired 2 days ago (returned), one expired
  60 days ago (excluded by `from`), one revoked and expiring tomorrow
  (excluded). Assert the returned public IDs and their order. Follow the
  package's existing testcontainers helpers.
- [ ] **Step 2: Run and watch it fail** — `go test ./internal/adapter/postgres/ -run Expiring -v`
- [ ] **Step 3: Add the port and implement the query**
- [ ] **Step 4: Run the tests** — Expected: PASS. Docker must be running; a
  skipped test is not a pass.
- [ ] **Step 5: Gates and commit**

```bash
just fmt && just vet && just build && just test-unit
git add internal/domain/governance/ports.go internal/adapter/postgres
git commit -F - <<'EOF'
Project-key expiry query

[+] governance.KeyExpiryReader port for the alerting sweep
[+] PostgreSQL ExpiringKeys over the existing schema
EOF
```

### Task 3.3 (integration): Reach the query from the worker seam — dispatched alone

**Files:**
- Modify: `internal/command/workers.go` (the `workerRepository` interface only)
- Test: `internal/command/workers_test.go`

**Interfaces:**
- Consumes: `Store.ExpiringKeys` from Task 3.2.
- Produces: the `workerRepository` shape frozen above. Task 5.1 schedules the
  sweep; this task only makes the method reachable.

- [ ] **Step 1:** Add `ExpiringKeys` to `workerRepository`. `productionServeDependencies`
  already passes `store.postgres`, which now satisfies it — confirm the tree
  builds and update any existing worker test double.
- [ ] **Step 2:** Add a test proving a double implementing the full interface
  satisfies `StartWorkers`, so a later signature drift fails here rather than in
  Batch 5.
- [ ] **Step 3:** Run `just fmt && just vet && just build && just test-unit`.
- [ ] **Step 4: Commit**

```bash
git add internal/command
git commit -F - <<'EOF'
Expose key expiry to the worker repository

[&] workerRepository carries ExpiringKeys for the alerting sweep
EOF
```

---

# Batch 4 — Observation points in the SDK adapter

### Task 4.1: Usage plugin and middleware observation — `same-agent`, **model: opus**

**Files:**
- Create: `internal/adapter/cliproxy/alert_plugin.go`
- Modify: `internal/adapter/cliproxy/middleware.go`
- Modify: `internal/adapter/cliproxy/helpers_test.go`,
  `internal/adapter/cliproxy/middleware_readiness_test.go` (its own package's
  `NewMiddleware` call sites)
- Test: `internal/adapter/cliproxy/alert_plugin_test.go`

**Reduced gate — this task only:**

```bash
just fmt \
  && go vet ./internal/adapter/cliproxy/... \
  && go build ./internal/adapter/cliproxy/... \
  && go test -race ./internal/adapter/cliproxy/...
```

`just vet`, `just build` and `just test-unit` all fail until Task 4.2 updates
`internal/command/serve.go` and the integration harness, which are
composition-root files an implementation task must not touch. `just vet` is
`go vet ./...` and type-checks the whole tree, so it must be narrowed too — not
merely skipped.

**Interfaces:**
- Consumes: `alert.Tracker`, `sdkusage.Record`, `governance.Admission`,
  `governance.KeyIdentity`.
- Produces: `NewAlertUsagePlugin` and the fifth `NewMiddleware` parameter.

**Design notes — the placement of each call matters:**

`alertUsagePlugin.HandleUsage` maps one record to one
`ObserveAttempt(record.Provider, record.AuthID, record.Model, record.Failed, record.Fail.StatusCode)`
and does nothing else. It must not touch the usage bridge, must not persist, and
must not panic on a zero-valued record. It is registered through `NewService`'s
existing extra-plugin slot, which already wraps it in `nonBarrierUsagePlugin`,
so barrier and cancellation control records never reach it. **Do not modify
`UsagePlugin`** — `validateServiceComposition` rejects a second `*UsagePlugin`,
not a distinct plugin type.

In `middleware.go`:

- `NewMiddleware` gains `tracker *alert.Tracker`, stored as a field. Nil is
  legal.
- `recordRequest`: on a repository error, call `ObserveDatabase(false)` **only
  when `c.Request.Context().Err() == nil`**; on success call
  `ObserveDatabase(true)`.
- `admit`: call
  `ObserveAdmission(keyIdentity.ProjectName, admission.Blocks, admission.Warnings)`
  **for `RouteGeneration` only, immediately after `recordRequest` returns ok and
  before the `!admission.Allowed` abort**. Placing it after the abort would mean
  only allowed admissions are ever observed and `budget_blocked` could never
  fire. The project name comes from the `keyIdentity governance.KeyIdentity`
  parameter, not from `RequestIdentity`, which has no name field.
- `admit`: for `RouteGeneration`, register a deferred
  `ObserveGeneration(c.Writer.Status())` alongside the existing barrier defer,
  so the status observed is the final one. Metadata requests are never observed
  and `complete`'s signature does not change.
- `complete`: on failure call `ObserveDatabase(false)` unconditionally — this
  path runs under `context.WithoutCancel`, so a deadline here is genuine
  slowness; on success call `ObserveDatabase(true)`.

- [ ] **Step 1: Write the failing tests**

A record with status 429 reaches `ObserveAttempt` with the right provider,
credential, model and status. A zero-valued record does not panic. A middleware
built with a nil tracker serves a request normally. A denied generation
admission still reports its blocks. A repository failure with a cancelled
request context does not mark the database down; the same failure with a live
context does.

- [ ] **Step 2: Run and watch them fail** — `go test ./internal/adapter/cliproxy/ -v`
- [ ] **Step 3: Implement the plugin, then the four middleware call sites**
- [ ] **Step 4: Run the package** — `go test -race ./internal/adapter/cliproxy/...`
  Expected: PASS.
- [ ] **Step 5: Commit** (reduced gate above, `just fmt && just vet` still apply)

```bash
just fmt && go vet ./internal/adapter/cliproxy/... && go build ./internal/adapter/cliproxy/... && go test -race ./internal/adapter/cliproxy/...
git add internal/adapter/cliproxy
git commit -F - <<'EOF'
Alert observation in the SDK adapter

[+] alertUsagePlugin mapping SDK usage records to credential observations
[&] Middleware observes admissions before the budget abort, generation status and database health
EOF
```

### Task 4.2 (integration): Restore the build across the tree — dispatched alone

**Files:**
- Modify: `internal/command/serve.go` (pass `nil`; Batch 5 replaces it)
- Modify: `test/integration/harness_test.go:235` (pass `nil`; Batch 6 replaces it)

- [ ] **Step 1:** `grep -rn "NewMiddleware(" --include='*.go' .` and update every
  remaining call site.
- [ ] **Step 2:** Run the full gate plus the integration suite:
  `just fmt && just vet && just build && just test-unit && just test-integration`.
- [ ] **Step 3: Commit**

```bash
git add internal/command test/integration
git commit -F - <<'EOF'
Thread the alert tracker through middleware construction

[&] Remaining NewMiddleware call sites pass the new tracker parameter
EOF
```

---

# Batch 5 — Composition root and operator commands

### Task 5.1: Serve wiring, sweep schedule and operator events — `same-agent`, **model: opus**

**Files:**
- Create: `internal/command/alerting.go`
- Modify: `internal/command/serve.go`
- Modify: `internal/command/workers.go`
- Modify: `internal/command/key.go`
- Modify: `internal/command/auth.go`
- Modify: `internal/command/serve_test.go` (its own package's dependency stubs)
- Modify: `internal/command/workers_test.go` (the double Task 3.3 created —
  `StartWorkers` gains a parameter here, and the package does not compile until
  this file follows)

**Gate:** the full default gate. The integration harness still passes `nil` to
`NewMiddleware` and keeps compiling, so nothing is deferred.

**Interfaces:**
- Consumes: `config.Config.DiscordWebhookURL`, `discord.New`, `alert.New`,
  `alert.DefaultWindow`, `cliproxy.ListAuth`, `cliproxy.NewAlertUsagePlugin`,
  the `workerRepository` from Task 3.3.
- Produces: `newOperatorNotifier` and `(*operatorNotifier).emit` in
  `alerting.go`, plus the `serveDependencies` and `StartWorkers` signatures
  frozen above.

**Design notes:**

`runServeWith` owns construction and lifetime — it is the only function holding
the context, the configuration, `Getenv` and the deferred shutdown:

1. Resolve the URL. A resolution error fails `serve` (spec §7).
2. Disabled → `alert.New(nil, nil, alert.DefaultWindow, time.Now)` and one log
   line. Every downstream call site stays identical; there is no webhook to
   close, so guard the deferred `Close` on a nil `*discord.Webhook` (the frozen
   `Close` is nil-receiver-safe, so a typed nil pointer is fine — never store it
   in an interface variable).
3. Enabled → `discord.New(url, nil, time.Now)`; log one warning when the host is
   neither `discord.com` nor `discordapp.com`; build the label map from
   `cliproxy.ListAuth(ctx, cfg.Proxy.AuthDir)`, converting `[]cliproxy.AuthInfo`
   into `map[string]alert.CredentialLabel` keyed by ID. **Include disabled
   entries** — the map only renders names, and a disabled credential's events
   should still read well. A `ListAuth` error logs a warning and yields a nil
   map; it never fails startup.
4. Pass the tracker into `deps.buildService` and `deps.startWorkers`.
5. `Emit(KindGatewayStarted)` once the service is constructed.
6. In the deferred block, after the lock release: `Emit(KindGatewayStopping)`
   carrying a **bounded classification**, never the raw error — one of
   `context_cancelled`, `service_returned`, `startup_failure`. `returnErr` can
   wrap store and lock errors carrying DSN material, and spec §12 forbids that
   reaching Discord. Then `webhook.Close` with
   `context.WithTimeout(context.Background(), 5*time.Second)` — **not** the
   serve context, which is already cancelled by the time this runs, exactly as
   the neighbouring lock-release defer already does.
7. `buildServeService` passes the tracker to `NewMiddleware`, registers
   `cliproxy.NewAlertUsagePlugin(tracker)` as the extra plugin of `NewService`,
   and emits `KindUsagePoisoned` from the existing `ReportPoisonWith` callback
   before stopping the service.

`workers.go` gains a third schedule: the key sweep, once immediately then
hourly, calling `ExpiringKeys(ctx, now-30d, now+7d)` then
`ObserveProjectKeys(keys, now)`. A sweep error calls `ObserveDatabase(false)`
and logs. `reconcile` and `pruneCompleted` call `ObserveDatabase(true)` on
success and `ObserveDatabase(false)` on error.

The operator notifier is deliberately **two-phase**, because one call cannot be
both "resolve before acting" and "emit after acting":

- `newOperatorNotifier(cfg, streams)` resolves the URL and returns an error. The
  four commands call it **first**, before `openStore`, before the key is
  created, before the login runs — so a malformed variable can never leave a
  half-done action (spec §7, §14). When alerting is disabled it returns a
  non-nil no-op notifier and a nil error, so call sites never branch.
- `(*operatorNotifier).emit(kind, fields...)` runs **after** the action, when
  the created key or the login result exists. It builds the webhook and a
  tracker with an empty label map, emits, and closes with a 5 s budget. It
  returns nothing: a delivery failure writes one warning to `streams.Err` and
  the command still succeeds (spec §11).

Call sites: `runKeyCreate` (`key.go`), `runKeyRotate`, `runAuthLogin`
(`auth.go`), `runAuthImportLegacy`.

- [ ] **Step 1:** Implement `alerting.go`, then serve, then workers, then the
  four command call sites, then the `serve_test.go` and `workers_test.go`
  stubs.
- [ ] **Step 2:** Run `just fmt && just vet && just build && just test-unit`.
  Expected: PASS.
- [ ] **Step 3: Commit**

```bash
git add internal/command
git commit -F - <<'EOF'
Wire alerting into serve, workers and operator commands

[+] Notifier and tracker construction in runServeWith with a 5s shutdown drain
[+] Hourly project-key expiry sweep on the existing worker goroutine
[+] Operator events for key create, key rotate, auth login and auth import-legacy
[&] serveDependencies and StartWorkers carry the tracker
EOF
```

### Task 5.2 (integration): Tests for the composition — dispatched alone, **model: opus**

**Files:**
- Create: `internal/command/alerting_test.go`
- Modify: `internal/command/workers_test.go`

- [ ] **Step 1: Write the tests**

`runServeWith` is dependency-injected and therefore directly testable: with an
unset variable it builds and runs without touching the network; with a stub
webhook URL it emits `gateway_started` before `Run` and `gateway_stopping`
after, and the stopping event's fields carry a bounded classification, never a
DSN; a malformed URL fails it; a valid non-Discord host logs one warning and
still runs. Point the fixture's `AuthDir` at `t.TempDir()` — enabling the
webhook makes `runServeWith` call `ListAuth`, which creates the directory, and
the current fixture's `/safe/auth` would be created at the filesystem root when
the suite runs as root in CI.

The `key`/`auth` commands have **no dependency-injection seam** — they call
`openStore`, which reads a real config file and opens a real pool — and building
one is out of this task's scope. Test the contract where it lives instead:
`newOperatorNotifier` returns an error for a malformed URL and a working no-op
notifier when the variable is unset, and `emit` against an unreachable server
writes exactly one warning to `streams.Err` and returns. That a command resolves
before it acts is guaranteed by call ordering, verified in review of Task 5.1's
diff rather than by a container fixture — record this in the batch synthesis as
the one spec §14 case covered by construction instead of by test.

The sweep passes `now-30d` and `now+7d` and forwards what the repository
returns.

- [ ] **Step 2:** Run the full gate:
  `just fmt && just vet && just build && just test-unit && just test-integration`.
- [ ] **Step 3: Commit**

```bash
git add internal/command
git commit -F - <<'EOF'
Composition tests for alerting

[+] Serve lifecycle, malformed-URL and delivery-failure cases
[+] Sweep window boundaries
EOF
```

---

# Batch 6 — Integration suite and documentation

### Task 6.1: Alert sink and harness wiring — `same-agent`, **model: opus**

**Files:**
- Modify: `test/integration/harness_test.go`
- Create: `test/integration/alerts_test.go` (the sink and its helpers only;
  Task 6.2 adds the cases)

**Design notes:**

The suite runs one service for the whole package, so the harness builds the
tracker with a **zero anti-flap window** — otherwise the first 429 in the
process would be the only observable one, and `usage_test.go` already forces
one.

The sink is an in-process `alert.Notifier` collecting events behind a mutex —
**not** an `httptest.Server` fronted by a real `discord.Webhook`. Three reasons:
`discord.New` applies the production 1 s inter-delivery throttle and
`newWithTimings` is unexported, so a real webhook would add seconds of latency
between assertions; nothing would own closing the server and the delivery
goroutine at teardown, while `Harness.checkLogLeaks` fails the package on any
registered secret logged after `Close`; and the HTTP layer already has
exhaustive coverage in Tasks 2.1 to 2.3. What this suite must prove is that the
real request path produces the right events — which is exactly what a sink
observes.

> **Spec divergence to report in the batch synthesis:** spec §14 says "the real
> gateway against a stub webhook server". The gateway and the events are real;
> only the transport is replaced. No user-visible behaviour changes.

```go
// Wait returns the first event of that kind, or false on timeout.
func (a *AlertSink) Wait(kind alert.Kind, timeout time.Duration) (alert.Event, bool)

// CountFor returns how many events of that kind carry a field with the value.
func (a *AlertSink) CountFor(kind alert.Kind, contains string) int
```

`CountFor` is what makes "no second message" assertable; `Wait` alone cannot
express an absence.

`startService` builds `alert.New(h.Alerts, labels, 0, time.Now)`, passes the
tracker to `NewMiddleware`, and registers `cliproxy.NewAlertUsagePlugin(tracker)`
alongside the existing `h.Usage` plugin.

Only what the request path produces is reachable here. Startup, shutdown, sweep
and operator events are covered by Task 5.2 and must not be asserted from this
suite.

- [ ] **Step 1:** Add the sink, its helpers and the wiring.
- [ ] **Step 2:** Run `just test-integration` to prove the suite still passes
  with alerting enabled and nothing asserted yet.
- [ ] **Step 3: Commit**

```bash
git add test/integration
git commit -F - <<'EOF'
Alert sink in the integration harness

[+] Collecting alert sink with kind-aware wait and count helpers
[&] Harness wires the alert tracker with a zero anti-flap window
EOF
```

### Task 6.2: Alert integration coverage — `same-agent`, **model: opus**

**Files:**
- Modify: `test/integration/alerts_test.go`

**Design note on the "no second message" case:** the suite's config declares two
round-robin API-key credentials, and credential state is keyed by (credential,
model), so a retried 429 can legitimately land on the *other* credential and
produce a second, correct message. Assert instead that the **same credential
identifier** taken from the first event appears exactly once, via `CountFor`.

Drive the alias `cooldown-other-model`, already declared in the harness config
and referenced by no test since commit `7efb999`. It must not be an alias
another test drives — `usage_test.go` and `budgets_test.go` would otherwise
share the entity in the same process and make the count depend on file order.

- [ ] **Step 1: Write the tests**

A stub upstream forced to 429 produces one `credential_rate_limited` event
naming the provider and the model; the same credential never appears twice. A
project whose `calls` budget is exhausted produces one `budget_blocked` naming
the project, dimension and window. Repeated upstream 5xx produce
`generation_failures` on the third admitted generation and not before.
Assertions check kind, severity and the presence of identifying fields — never
exact wording, and **never an order between two `budget_cleared` events** of the
same project, which the engine does not define.

- [ ] **Step 2:** Run `just test-integration`. Retry transient upstream errors
  with bounded backoff; never retry the gateway's own 402 or 503, which are the
  assertions.
- [ ] **Step 3: Commit**

```bash
git add test/integration
git commit -F - <<'EOF'
Integration coverage for alerting

[+] credential_rate_limited, budget_blocked and generation_failures cases
[+] Same-credential count case proving one message per transition
EOF
```

### Task 6.3 (integration): Documentation and the full gate — dispatched alone

**Files:**
- Modify: `.env.example`, `config.example.yaml`, `README.md`

- [ ] **Step 1:** Add `LLMGW_DISCORD_WEBHOOK_URL=` to `.env.example` — **empty,
  not `CHANGE_ME`**: the README tells operators to copy the file verbatim, and
  `CHANGE_ME` is not an absolute URL, so it would fail `serve`. Add
  `discord-webhook-url-env: LLMGW_DISCORD_WEBHOOK_URL` under `llmgw:` in
  `config.example.yaml`. Add the variable to the README's deployment-inputs list
  (around line 49), stating that empty disables alerting and that an enabled
  webhook needs outbound HTTPS to `discord.com` from the container (spec §15).
- [ ] **Step 2:** Run the whole gate: `just test`.
- [ ] **Step 3:** Confirm no real webhook URL is anywhere in the tree:
  `grep -rn "discord.com/api/webhooks" . --include='*' | grep -v '^./docs/'`
  must return nothing but shape examples without a token.
- [ ] **Step 4: Commit**

```bash
git add .env.example config.example.yaml README.md
git commit -F - <<'EOF'
Document the Discord alerting variable

[+] LLMGW_DISCORD_WEBHOOK_URL in .env.example, empty by default
[+] discord-webhook-url-env in the example configuration
[&] README deployment inputs list the optional webhook variable
EOF
```

---

## Spec Coverage

| Spec section | Task |
|---|---|
| §5.1 domain surface, observation contract | 1.1, 4.1 |
| §5.2 transition semantics | 1.1, 1.2 |
| §5.3 credential labels, best-effort inventory | 5.1 (map construction), 1.1 (ID fallback) |
| §6.1 credentials, client-4xx exclusion | 1.1, 1.2, 4.1, 6.2 |
| §6.2 generation health | 1.1, 1.2, 4.1, 6.2 |
| §6.3 project keys, bounded window, short-lifetime skip | 1.1, 3.2, 3.3, 5.1 |
| §6.4 budgets, generation-only observation | 1.1, 1.2, 4.1, 6.2 |
| §6.5 gateway health, atomic-flag guard | 1.1, 4.1, 5.1 |
| §6.6 operator actions | 5.1, 5.2 |
| §7 configuration, lazy resolution, command failure | 3.1, 5.1, 5.2, 6.3 |
| §8 rendering | 2.1 |
| §9 delivery, retry, newest-first drain | 2.2, 2.3 |
| §10 wiring | 5.1, 5.2 |
| §11 operator commands, delivery failure is a warning | 5.1, 5.2 |
| §12 privacy | 1.1 (field sets), 1.2 (assertion), 5.1 (bounded stop reason) |
| §13 limitations | documentation only, no task |
| §14 testing | 1.2, 2.3, 4.1 (the observation-contract cases), 5.2, 6.1, 6.2 |
| §15 deployment | 6.3 |
