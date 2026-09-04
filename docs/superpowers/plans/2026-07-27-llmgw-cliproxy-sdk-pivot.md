# LLMGW CLIProxyAPI SDK Pivot Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace LLMGW's native provider proxy with one embedded CLIProxyAPI SDK server while retaining secure project keys, exact call admission, token/cost accounting, and local administration in the same binary.

**Architecture:** `llmgw` loads one YAML file, opens PostgreSQL, installs one global governance middleware and one exclusive SDK access provider, then runs CLIProxyAPI in-process. The middleware owns project authentication and request admission; a CLIProxyAPI usage plugin turns each upstream attempt into durable accounting. The local CLI talks directly to PostgreSQL or CLIProxyAPI's public auth SDK and exposes no management HTTP API.

**Tech Stack:** Go 1.26, CLIProxyAPI `github.com/router-for-me/CLIProxyAPI/v7@v7.2.102`, Gin middleware through the SDK, PostgreSQL 16 with pgx v5, YAML v3, testcontainers-go, Go standard `flag` package.

## Global Constraints

- Pin CLIProxyAPI to tag `v7.2.102`, commit `8423cce2d1004e80948a9e2c60ee69354c0aabc3`; upgrades are separate reviewed changes.
- Keep one shipped binary, one server process, one container, one public listener, and no child CLIProxyAPI process or reverse proxy.
- Keep one operator YAML file: native CLIProxyAPI fields at the root and LLMGW fields under `llmgw:`.
- `LLMGW_POSTGRES_DSN` and `LLMGW_KEY_PEPPER` remain environment/secret-manager values; the YAML stores only their environment variable names.
- One project key belongs to exactly one project; a project may have multiple keys. Remove tags, `X-Project`, `X-Tags`, per-project ports/hosts, and `LLMGW_DEFAULT_PROJECT`.
- Accept only `Authorization: Bearer <key>` and `x-api-key: <key>`; when both are present, require byte-for-byte equality.
- Reject top-level native CLIProxyAPI `api-keys`; install the LLMGW SDK access provider with `sdk/access.SetExclusiveProvider`.
- Hard-deny CLIProxyAPI management, control-panel, root-information, and main-server OAuth callback routes even if a later config reload attempts to enable them.
- Disable CLIProxyAPI request/error body capture by providing a nil request logger. Never log raw project keys, prompts, provider credentials, `usage.Record.Source`, failure bodies, or response headers.
- Count an authenticated generation request once before dispatch. Calls are strict; token and cost limits are stop-after-crossing and may overshoot under concurrent in-flight requests.
- Treat cost as notional USD, not an upstream invoice. Reasoning tokens are an output breakdown and are never charged twice.
- Run one active LLMGW instance per PostgreSQL database/auth directory in this version.
- Treat migration `0010` and the runtime cutover as one stopped-traffic release; intermediate commits are review checkpoints, not deployable versions against the migrated database.
- Unit-test deterministic LLMGW logic and thin middleware/CLI orchestration only. Exercise proxy routes, retries, streaming, context propagation, and account selection against the real pinned SDK with deterministic upstreams.
- Follow `CLAUDE.md`: short documented functions, focused files, hexagonal boundaries, contextual wrapped errors, no committed `replace` directive, and repository-specific multiline commit messages without conventional prefixes.

---

## Target File Map

### Composition and commands

- Modify `cmd/llmgw/main.go`: reduce it to signal setup, `command.Run`, error printing, and exit status.
- Create `internal/command/root.go`: subcommand dispatch and shared `--config` resolution.
- Create `internal/command/config.go`: command streams, config-path lookup, and PostgreSQL opening.
- Create `internal/command/serve.go`: server composition and background-worker lifecycle.
- Create `internal/command/key.go`: `key create|list|rotate|revoke`.
- Create `internal/command/budget.go`: `budget set|list|delete`.
- Create `internal/command/usage.go`: `usage show|resolve`.
- Create `internal/command/auth.go`: `auth login|list|import-legacy`.
- Create `internal/command/*_test.go`: command parsing/output tests using real repositories where persistence matters.

### Configuration and domain

- Rewrite `internal/config/config.go`: one-file loader, environment secret resolution, and security validation.
- Rewrite `internal/config/config_test.go`: YAML, env, and forbidden-setting tests.
- Create `internal/domain/governance/key_types.go`: project and key values.
- Create `internal/domain/governance/budget_types.go`: limit, window-total, breach, and admission values.
- Create `internal/domain/governance/accounting_types.go`: request, attempt, token, pricing, and reconciliation values.
- Create `internal/domain/governance/reporting_types.go`: usage query/summary and legacy credential values.
- Create `internal/domain/governance/ports.go`: narrow persistence ports consumed by services/adapters.
- Create `internal/domain/projectkey/token.go`: key format, generation, parsing, and HMAC digest.
- Create `internal/domain/projectkey/service.go`: create, authenticate, rotate, and revoke flows.
- Create `internal/domain/projectkey/*_test.go`: pure key tests.
- Create `internal/domain/governance/budget/evaluator.go`: project-only admission evaluation without colliding with the legacy runtime package.
- Create `internal/domain/governance/budget/evaluator_test.go`: pure limit arithmetic.
- Create `internal/domain/governance/cost/calculator.go`: canonical notional-cost calculation without colliding with the legacy runtime package.
- Create `internal/domain/governance/cost/calculator_test.go`: pure pricing tests.

### PostgreSQL

- Add `internal/adapter/postgres/migrations/0010_cliproxy_governance.sql`: archive the legacy accounting schema and create the new schema.
- Simplify `internal/adapter/postgres/store.go`: pool lifecycle only.
- Create `internal/adapter/postgres/keys.go`: project/key persistence.
- Create `internal/adapter/postgres/admission.go`: advisory-lock admission and request lifecycle.
- Create `internal/adapter/postgres/budgets.go`: budget administration.
- Create `internal/adapter/postgres/attempts.go`: price lookup and attempt persistence.
- Create `internal/adapter/postgres/reconcile.go`: interrupted/pending accounting recovery and explicit resolution.
- Create `internal/adapter/postgres/reporting.go`: usage summaries.
- Create `internal/adapter/postgres/legacy_auth.go`: read-only legacy credential export.
- Create `internal/adapter/postgres/governance_retention.go`: prune completed request trees without removing the legacy pruner before cutover.
- Create/add matching `*_test.go` files with PostgreSQL testcontainers.

### CLIProxyAPI adapter

- Create `internal/adapter/cliproxy/routes.go`: public, denied, metadata, and generation route classification.
- Create `internal/adapter/cliproxy/context.go`: immutable request identity context helpers.
- Create `internal/adapter/cliproxy/access.go`: exclusive SDK access provider.
- Create `internal/adapter/cliproxy/middleware.go`: auth, admission, final status, and hard-deny middleware.
- Create `internal/adapter/cliproxy/usage.go`: SDK record mapping and durable usage plugin.
- Create `internal/adapter/cliproxy/service.go`: SDK builder and lifecycle wrapper.
- Create `internal/adapter/cliproxy/auth.go`: public SDK OAuth/list/import helpers.
- Create `internal/adapter/cliproxy/*_test.go`: pure classification/mapping and LLMGW middleware-orchestration tests; real proxy behavior stays in integration.

### Integration, deployment, and cleanup

- Replace `test/e2e/` with gated live tests for the embedded SDK.
- Create `test/integration/main_test.go`, `harness_test.go`, `stub_upstream_test.go`, and focused protocol/governance test files.
- Delete `internal/adapter/httpserver/`, `internal/adapter/provider/`, legacy `internal/domain/{budget,llm,usage}/`, and obsolete `internal/domain/{errors.go,ports.go}` only after the embedded service passes parity tests.
- Create `config.example.yaml`.
- Update `.env.example`, `README.md`, `Dockerfile`, `docker-compose.yml`, `CLAUDE.md`, and `.github/workflows/ci.yml`; remove the obsolete provider-specific `.github/workflows/e2e-codex.yml`.
- Create `third_party/CLIProxyAPI/LICENSE` and `THIRD_PARTY_NOTICES.md`.

---

### Task 1: Pin the SDK and load one secure YAML configuration

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `cmd/llmgw/main.go`
- Rewrite: `internal/config/config.go`
- Rewrite: `internal/config/config_test.go`
- Create temporarily: `internal/config/legacy.go`
- Create: `config.example.yaml`

**Interfaces:**
- Produces: `config.Load(path string, getenv func(string) string) (config.Config, error)`
- Produces: `config.Config.DatabaseDSN(getenv func(string) string) (string, error)`
- Produces: `config.Config.KeyPepper(getenv func(string) string) ([]byte, error)`
- Produces: `config.Config.Proxy *sdkconfig.Config`, `config.Config.Path string`, `config.Config.LLMGW config.LLMGW`
- Preserves temporarily: `config.LoadLegacy() (config.LegacyConfig, error)` for the old composition root
- Consumes later: every command and `cliproxy.NewService`

- [ ] **Step 1: Add failing configuration tests**

Cover the shared YAML, secret indirection, unknown `llmgw` block compatibility, and every security rejection:

```go
func TestLoadSharedConfig(t *testing.T) {
	path := writeConfig(t, `
host: 127.0.0.1
port: 8088
auth-dir: /tmp/auth
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
llmgw:
  postgres-dsn-env: TEST_DSN
  key-pepper-env: TEST_PEPPER
  usage-retention-days: 35
`)
	cfg, err := Load(path, mapEnv(map[string]string{
		"TEST_DSN":    "postgres://example",
		"TEST_PEPPER": strings.Repeat("p", 32),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.Host != "127.0.0.1" || cfg.Proxy.Port != 8088 {
		t.Fatalf("proxy address = %s:%d", cfg.Proxy.Host, cfg.Proxy.Port)
	}
	if cfg.UsageRetention != 35*24*time.Hour {
		t.Fatalf("retention = %s", cfg.UsageRetention)
	}
	if got, err := cfg.DatabaseDSN(mapEnv(map[string]string{"TEST_DSN": "postgres://example"})); err != nil || got != "postgres://example" {
		t.Fatalf("database DSN = %q, %v", got, err)
	}
	if got, err := cfg.KeyPepper(mapEnv(map[string]string{"TEST_PEPPER": strings.Repeat("p", 32)})); err != nil || len(got) != 32 {
		t.Fatalf("key pepper length = %d, %v", len(got), err)
	}
}
```

Use table cases that must fail for:

```text
top-level api-keys non-empty
remote-management.allow-remote true
remote-management.secret-key non-empty
remote-management.disable-control-panel false
home.enabled true
pprof.enable true
MANAGEMENT_PASSWORD non-empty
auth-dir empty
DatabaseDSN called with a missing PostgreSQL environment value
KeyPepper called with a missing pepper environment value
KeyPepper called with a pepper shorter than 32 bytes
usage-retention-days below 2
```

For every rejected file, assert its bytes are unchanged; in particular, CLIProxyAPI's loader must never get a chance to replace a plaintext management secret with a bcrypt value before LLMGW rejects it.

- [ ] **Step 2: Run the configuration tests and verify failure**

Run: `go test ./internal/config -run 'TestLoad|TestSecurity|TestSecrets' -count=1`

Expected: FAIL because the current environment-only `Load()` has the wrong signature and no YAML support.

- [ ] **Step 3: Pin CLIProxyAPI and implement the loader**

Run: `go get github.com/router-for-me/CLIProxyAPI/v7@v7.2.102`

Implement these exact public shapes:

```go
type LLMGW struct {
	PostgresDSNEnv    string `yaml:"postgres-dsn-env"`
	KeyPepperEnv      string `yaml:"key-pepper-env"`
	UsageRetentionDays int   `yaml:"usage-retention-days"`
}

type Config struct {
	Path           string
	Proxy          *sdkconfig.Config
	LLMGW          LLMGW
	UsageRetention time.Duration
}

func Load(path string, getenv func(string) string) (Config, error)
func (c Config) DatabaseDSN(getenv func(string) string) (string, error)
func (c Config) KeyPepper(getenv func(string) string) ([]byte, error)
```

`Load` must first read the file bytes and decode a narrow security projection containing `llmgw`, `api-keys`, `remote-management`, `home`, and `pprof`. Validate that projection and `MANAGEMENT_PASSWORD` before calling `sdkconfig.LoadConfig`; the pinned SDK hashes and rewrites non-empty management secrets, so validation order is a security boundary. Only after the raw file is accepted may `Load` call the SDK loader for the complete native configuration. Default the environment names to `LLMGW_POSTGRES_DSN` and `LLMGW_KEY_PEPPER` and retention to 35 days. `DatabaseDSN` and `KeyPepper` resolve secrets only for commands that need them; both return contextual errors without including secret values. `KeyPepper` rejects values shorter than 32 bytes. This separation lets local OAuth/list commands work without database or pepper access.

Move the current environment-only `Config`, parsing helpers, and `Load()` behavior into `legacy.go` as `LegacyConfig` and `LoadLegacy`. While moving it, replace malformed session-key/Codex-account errors that currently quote the raw input with index-only redacted errors and add a regression assertion. Change only the old `cmd/llmgw/main.go` load call and its `serve` parameter type to `LoadLegacy`/`config.LegacyConfig`, so the existing native server continues to compile until Task 12 performs the atomic cutover. No new code may consume the legacy API, and Task 12 deletes it.

The checked-in example must contain the account-pool defaults from the spec:

```yaml
host: 127.0.0.1
port: 8088
auth-dir: ./var/cliproxy-auth

remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true

request-log: false
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

- [ ] **Step 4: Verify the loader and module pin**

Run:

```bash
go mod tidy
go test ./internal/config -count=1
go test ./cmd/llmgw ./internal/... -run '^$'
go list -m github.com/router-for-me/CLIProxyAPI/v7
rg '^replace ' go.mod
```

Expected: tests PASS; module output ends in `v7.2.102`; `rg` returns no match.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum cmd/llmgw/main.go internal/config config.example.yaml
git commit -m "Load embedded proxy configuration

[+] pin CLIProxyAPI v7.2.102 SDK
[+] parse one proxy and governance YAML file
[!] reject management and alternate inbound auth settings"
```

---

### Task 2: Migrate to the project-key and request-attempt schema

**Files:**
- Create: `internal/adapter/postgres/migrations/0010_cliproxy_governance.sql`
- Create: `internal/domain/governance/key_types.go`
- Create: `internal/domain/governance/budget_types.go`
- Create: `internal/domain/governance/accounting_types.go`
- Create: `internal/domain/governance/reporting_types.go`
- Create: `internal/domain/governance/ports.go`
- Create: `internal/adapter/postgres/migration_cliproxy_test.go`
- Create: `internal/adapter/postgres/governance_test_helpers_test.go`

**Interfaces:**
- Produces: all values and repository interfaces in package `governance`
- Produces: archived `legacy_usage_event`, `legacy_budget_limit`, and `legacy_model_price`
- Produces: new `client_key`, `budget_limit`, `request_event`, `usage_attempt`, and `model_price`
- Consumes: Task 1 configuration only through the composition root, not in domain code

- [ ] **Step 1: Define the domain values before writing SQL**

Create documented string types and constants:

```go
type Dimension string
const (
	DimensionCalls  Dimension = "calls"
	DimensionTokens Dimension = "tokens"
	DimensionCost   Dimension = "cost"
)

type Window string
const (
	WindowHour Window = "hour"
	WindowDay  Window = "day"
)

type Action string
const (
	ActionBlock Action = "block"
	ActionWarn  Action = "warn"
)

type Operation string
const (
	OperationGeneration Operation = "generation"
	OperationMetadata   Operation = "metadata"
)

type RequestState string
const (
	RequestInFlight RequestState = "in_flight"
	RequestCompleted RequestState = "completed"
)

type AccountingState string
const (
	AccountingPending       AccountingState = "pending"
	AccountingObserved      AccountingState = "observed"
	AccountingUnknown       AccountingState = "accounting_unknown"
	AccountingResolvedZero  AccountingState = "resolved_zero"
	AccountingNotApplicable AccountingState = "not_applicable"
)

type PricingState string
const (
	PricingPriced  PricingState = "priced"
	PricingUnknown PricingState = "unknown_pricing"
)
```

Define the shared values with these field names so every later task compiles against one vocabulary:

```go
type Project struct {
	ID int64
	Name string
	CreatedAt time.Time
}

type ClientKey struct {
	ID int64
	ProjectID int64
	ProjectName string
	Name string
	PublicID string
	Digest []byte
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
	LastUsedAt *time.Time
}

type KeyInfo struct {
	ID int64
	ProjectID int64
	ProjectName string
	Name string
	PublicID string
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
	LastUsedAt *time.Time
}

type KeyIdentity struct {
	ProjectID int64
	ProjectName string
	ClientKeyID int64
	KeyName string
	PublicID string
}

type CreatedKey struct {
	Key KeyInfo
	Plaintext string
}

type BudgetLimit struct {
	ID int64
	ProjectID int64
	Dimension Dimension
	Window Window
	MaxValue float64
	Action Action
	CreatedAt time.Time
	UpdatedAt time.Time
}

type RequestEvent struct {
	ID string
	ProjectID int64
	ClientKeyID int64
	Operation Operation
	RequestedAt time.Time
	CompletedAt *time.Time
	Method string
	Path string
	RequestedModel *string
	State RequestState
	AccountingState AccountingState
	DownstreamStatus *int
	AccountingResolvedAt *time.Time
}

type TokenBreakdown struct {
	UncachedInput int64
	CacheRead int64
	CacheCreation int64
	Output int64
	Reasoning int64
	Total int64
	Unclassified int64
}

type UsageAttempt struct {
	ID string
	RequestID string
	Provider string
	ExecutorType string
	ResolvedModel string
	RequestedAlias string
	UpstreamAuthID string
	UpstreamAuthType string
	Tokens TokenBreakdown
	ServiceTier string
	ResponseServiceTier string
	Failed bool
	UpstreamStatus *int
	Latency time.Duration
	TTFT time.Duration
	CostUSD *float64
	PricingState PricingState
	CreatedAt time.Time
}

type PriceRule struct {
	ID int64
	Provider string
	ModelPattern string
	ServiceTier string
	InputPerMillion *float64
	OutputPerMillion *float64
	CacheReadPerMillion *float64
	CacheCreationPerMillion *float64
	EffectiveFrom time.Time
}

type WindowTotals struct {
	Calls int64
	Tokens int64
	CostUSD float64
	UnknownPricing int64
	UnknownAccounting int64
	CallsResetAt time.Time
	TokensResetAt time.Time
	CostResetAt time.Time
}

type BudgetBreach struct {
	Limit BudgetLimit
	ResetAt time.Time
}

type Admission struct {
	Allowed bool
	Request RequestEvent
	Blocks []BudgetBreach
	Warnings []BudgetBreach
}

type UsageQuery struct {
	Project string
	Since time.Time
	GroupBy string
}

type UsageSummary struct {
	Group string
	Calls int64
	Tokens int64
	CostUSD float64
	FailedAttempts int64
	UnknownPricing int64
	UnknownAccounting int64
}

type ReconcileResult struct {
	Observed int64
	Unknown int64
}

type LegacyCredential struct {
	Provider string
	AccountLabel string
	AccessToken string
	RefreshToken string
	SessionKey string
	ChatGPTAccountID string
	ExpiresAt *time.Time
}
```

Use `int64` database IDs, string UUIDs, UTC `time.Time`, nullable database values as pointers, and `[]byte` only for key digests. Add Go documentation to every exported type, field, constant, and method as required by `CLAUDE.md`.

- [ ] **Step 2: Add a failing upgrade-migration test**

Add `newGovernanceStore(t)` in `governance_test_helpers_test.go`; it starts PostgreSQL 16 and opens `postgres.New`, applying every migration. All post-migration repository tests added by this plan use this helper. Do not reuse the legacy `newTestStore` in `store_test.go`, because that file is deleted with the native stack in Task 12. The upgrade-migration test itself starts a raw container and calls the package-private migration helpers one filename at a time so it can seed state after `0009` and before `0010`.

The test must apply migrations `0001` through `0009`, insert:

```sql
INSERT INTO project (name) VALUES ('analytics');
INSERT INTO budget_limit (project_id, tag, dimension, "window", max_value, action)
VALUES
  ((SELECT id FROM project WHERE name='analytics'), NULL, 'calls', 'hour', 50, 'block'),
  ((SELECT id FROM project WHERE name='analytics'), 'worker-a', 'calls', 'hour', 5, 'block');
INSERT INTO usage_event (project_id, tag, model, provider, status)
VALUES ((SELECT id FROM project WHERE name='analytics'), 'worker-a', 'legacy-model', 'claude_max', 'ok');
```

Then apply `0010` and assert:

```text
legacy_usage_event contains 1 row
legacy_budget_limit contains 2 rows
new budget_limit contains only the tag-null row and maps cost_usd to cost
reservation no longer exists
project still contains analytics
provider, route, and oauth_token still exist
new tables and CHECK constraints accept every declared governance enum
```

- [ ] **Step 3: Run the migration test and verify failure**

Run: `go test ./internal/adapter/postgres -run TestCLIProxyGovernanceMigration -count=1`

Expected: FAIL because migration `0010` and the new tables do not exist.

- [ ] **Step 4: Write the migration**

The migration must run transactionally through the existing migration runner and perform this order:

```sql
ALTER TABLE usage_event RENAME TO legacy_usage_event;
ALTER TABLE budget_limit RENAME TO legacy_budget_limit;
ALTER TABLE model_price RENAME TO legacy_model_price;
DROP TABLE reservation;

CREATE TABLE client_key (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    public_id TEXT NOT NULL UNIQUE,
    digest BYTEA NOT NULL CHECK (octet_length(digest) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);

CREATE TABLE budget_limit (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    dimension TEXT NOT NULL CHECK (dimension IN ('calls','tokens','cost')),
    "window" TEXT NOT NULL CHECK ("window" IN ('hour','day')),
    max_value DOUBLE PRECISION NOT NULL CHECK (
      max_value >= 0
      AND max_value < 'Infinity'::double precision
      AND (dimension = 'cost' OR max_value = trunc(max_value))
    ),
    action TEXT NOT NULL CHECK (action IN ('block','warn')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, dimension, "window", action)
);

CREATE TABLE request_event (
    id UUID PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    client_key_id BIGINT NOT NULL REFERENCES client_key(id),
    operation TEXT NOT NULL CHECK (operation IN ('generation','metadata')),
    requested_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    requested_model TEXT,
    state TEXT NOT NULL CHECK (state IN ('in_flight','completed')),
    accounting_state TEXT NOT NULL CHECK (
      accounting_state IN ('pending','observed','accounting_unknown','resolved_zero','not_applicable')
    ),
    downstream_status INTEGER,
    accounting_resolved_at TIMESTAMPTZ
);

CREATE TABLE usage_attempt (
    id UUID PRIMARY KEY,
    request_id UUID NOT NULL REFERENCES request_event(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    executor_type TEXT NOT NULL,
    resolved_model TEXT NOT NULL,
    requested_alias TEXT NOT NULL,
    upstream_auth_id TEXT NOT NULL,
    upstream_auth_type TEXT NOT NULL,
    input_tokens BIGINT NOT NULL CHECK (input_tokens >= 0),
    output_tokens BIGINT NOT NULL CHECK (output_tokens >= 0),
    reasoning_tokens BIGINT NOT NULL CHECK (reasoning_tokens >= 0),
    cache_read_tokens BIGINT NOT NULL CHECK (cache_read_tokens >= 0),
    cache_creation_tokens BIGINT NOT NULL CHECK (cache_creation_tokens >= 0),
    total_tokens BIGINT NOT NULL CHECK (total_tokens >= 0),
    unclassified_tokens BIGINT NOT NULL CHECK (unclassified_tokens >= 0),
    service_tier TEXT NOT NULL,
    response_service_tier TEXT NOT NULL,
    failed BOOLEAN NOT NULL,
    upstream_status INTEGER,
    latency_ms BIGINT NOT NULL CHECK (latency_ms >= 0),
    ttft_ms BIGINT NOT NULL CHECK (ttft_ms >= 0),
    cost_usd DOUBLE PRECISION CHECK (
      cost_usd >= 0 AND cost_usd < 'Infinity'::double precision
    ),
    pricing_state TEXT NOT NULL CHECK (pricing_state IN ('priced','unknown_pricing')),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE model_price (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider TEXT NOT NULL,
    model_pattern TEXT NOT NULL,
    service_tier TEXT NOT NULL,
    input_per_million DOUBLE PRECISION CHECK (
      input_per_million >= 0 AND input_per_million < 'Infinity'::double precision
    ),
    output_per_million DOUBLE PRECISION CHECK (
      output_per_million >= 0 AND output_per_million < 'Infinity'::double precision
    ),
    cache_read_per_million DOUBLE PRECISION CHECK (
      cache_read_per_million >= 0 AND cache_read_per_million < 'Infinity'::double precision
    ),
    cache_creation_per_million DOUBLE PRECISION CHECK (
      cache_creation_per_million >= 0 AND cache_creation_per_million < 'Infinity'::double precision
    ),
    effective_from TIMESTAMPTZ NOT NULL,
    UNIQUE (provider, model_pattern, service_tier, effective_from)
);
```

Copy only project-wide budgets and map legacy prices without guessing cache discounts:

```sql
INSERT INTO budget_limit (project_id, dimension, "window", max_value, action)
SELECT project_id,
       CASE dimension WHEN 'cost_usd' THEN 'cost' ELSE dimension END,
       "window", max_value, action
FROM legacy_budget_limit
WHERE tag IS NULL;

INSERT INTO model_price (
    provider, model_pattern, service_tier,
    input_per_million, output_per_million,
    cache_read_per_million, cache_creation_per_million,
    effective_from
)
SELECT '*', model, '*',
       input_usd_per_mtok, output_usd_per_mtok,
       NULL, NULL, '1970-01-01T00:00:00Z'::timestamptz
FROM legacy_model_price;
```

Add indexes:

```text
client_key(public_id)
budget_limit(project_id)
request_event(project_id, requested_at)
request_event(accounting_state, completed_at)
usage_attempt(request_id)
usage_attempt(created_at)
model_price(provider, service_tier, effective_from DESC)
```

- [ ] **Step 5: Add narrow repository ports**

In `governance/ports.go`, define:

```go
type KeyRepository interface {
	CreateKey(context.Context, string, string, string, []byte, *time.Time) (ClientKey, error)
	KeyByPublicID(context.Context, string) (ClientKey, error)
	RotateKey(context.Context, int64, string, []byte, time.Time, time.Duration) (ClientKey, error)
	ListKeys(context.Context, string) ([]ClientKey, error)
	MarkKeyUsed(context.Context, int64, time.Time) error
	RevokeKey(context.Context, int64, time.Time) error
	ExpireKey(context.Context, int64, time.Time) error
}

type RequestRepository interface {
	AdmitGeneration(context.Context, RequestEvent, time.Time) (Admission, error)
	RecordMetadata(context.Context, RequestEvent) error
	CompleteRequest(context.Context, string, int, time.Time) error
}

type BudgetRepository interface {
	SetBudget(context.Context, string, Dimension, Window, float64, Action) (BudgetLimit, error)
	ListBudgets(context.Context, string) ([]BudgetLimit, error)
	DeleteBudget(context.Context, int64) error
}

type UsageRepository interface {
	PriceRuleFor(context.Context, string, string, string, time.Time) (PriceRule, bool, error)
	RecordAttempt(context.Context, UsageAttempt) error
	RecoverInterrupted(context.Context, time.Time) (int64, error)
	ReconcileAccounting(context.Context, time.Time, time.Duration, time.Duration) (ReconcileResult, error)
	ResolveUnknownAsZero(context.Context, string, time.Time) error
	PruneCompletedRequests(context.Context, time.Duration) (int64, error)
	QueryUsage(context.Context, UsageQuery) ([]UsageSummary, error)
}
```

- [ ] **Step 6: Verify migration and domain compilation**

Run:

```bash
gofmt -w internal/domain/governance
go test ./internal/domain/governance ./internal/adapter/postgres -run TestCLIProxyGovernanceMigration -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/governance internal/adapter/postgres/migrations/0010_cliproxy_governance.sql internal/adapter/postgres/migration_cliproxy_test.go internal/adapter/postgres/governance_test_helpers_test.go
git commit -m "Create governance accounting schema

[+] archive tag-based usage budgets and prices
[+] add project keys request events and usage attempts
[+] define narrow governance persistence ports"
```

---

### Task 3: Implement secure project-key lifecycle

**Files:**
- Create: `internal/domain/projectkey/token.go`
- Create: `internal/domain/projectkey/token_test.go`
- Create: `internal/domain/projectkey/service.go`
- Create: `internal/domain/projectkey/service_test.go`
- Create: `internal/adapter/postgres/keys.go`
- Create: `internal/adapter/postgres/keys_test.go`

**Interfaces:**
- Consumes: `governance.KeyRepository`, `governance.ClientKey`, `governance.KeyInfo`, `governance.KeyIdentity`
- Produces: `projectkey.NewService(repo, pepper, random, now)`
- Produces: `Service.Create`, `Service.Authenticate`, `Service.Rotate`, `Service.Revoke`, `Service.List`
- Consumes later: governance middleware and key CLI

- [ ] **Step 1: Write pure failing token tests**

Assert:

```go
func TestTokenRoundTrip(t *testing.T) {
	token, err := Generate(strings.NewReader(strings.Repeat("r", 44)))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(token.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PublicID != token.PublicID || len(parsed.Secret) != 32 {
		t.Fatalf("parsed token = %#v", parsed)
	}
}
```

Also reject any value whose total encoded length is not exactly 68 bytes before allocating decode buffers, the wrong prefix, missing separators, padded base64, a public ID not decoding to 12 bytes, and a secret not decoding to 32 bytes. Assert `Digest` is deterministic and `hmac.Equal` rejects a one-byte change.

- [ ] **Step 2: Run token tests and verify failure**

Run: `go test ./internal/domain/projectkey -run 'TestToken|TestDigest' -count=1`

Expected: FAIL because package `projectkey` does not exist.

- [ ] **Step 3: Implement token generation and digesting**

Use:

```go
const Prefix = "llmgw_k_"

type Token struct {
	Plaintext string
	PublicID string
	Secret []byte
}

func Generate(random io.Reader) (Token, error)
func Parse(raw string) (Token, error)
func Digest(pepper []byte, raw string) [sha256.Size]byte
```

Read exactly 12 public-ID bytes and 32 secret bytes with `io.ReadFull`, encode both with `base64.RawURLEncoding`, and compute `HMAC-SHA-256(pepper, full key)`. Never include `raw` in an error.

- [ ] **Step 4: Write failing service and PostgreSQL tests**

Use a fixed clock/random reader and assert:

```text
Create creates the project only from the CLI service path.
Create trims project/key labels, rejects empty or invalid UTF-8/control-character values, and rejects labels longer than 128 bytes.
Create returns plaintext once; PostgreSQL stores only public_id and 32-byte digest.
Authenticate returns project/key identity and updates last_used_at.
Unknown, malformed, expired, revoked, and digest-mismatch keys all return ErrInvalidCredential.
Rotate creates a new key before immediately revoking the old key.
Rotate with overlap sets old expires_at to now+overlap and keeps both valid until then.
An injected failure while updating the old row rolls back the replacement insert, so no undisclosed active key remains.
List never exposes digest or plaintext.
List with an empty project returns every key; a non-empty project filters exactly.
```

- [ ] **Step 5: Implement the service and repository**

Use these service signatures:

```go
var ErrInvalidCredential = errors.New("invalid credential")

type Service struct {
	repo governance.KeyRepository
	pepper []byte
	random io.Reader
	now func() time.Time
}

func NewService(repo governance.KeyRepository, pepper []byte, random io.Reader, now func() time.Time) (*Service, error)
func (s *Service) Create(ctx context.Context, project, name string, expiresAt *time.Time) (governance.CreatedKey, error)
func (s *Service) Authenticate(ctx context.Context, raw string) (governance.KeyIdentity, error)
func (s *Service) Rotate(ctx context.Context, keyID int64, overlap time.Duration) (governance.CreatedKey, error)
func (s *Service) Revoke(ctx context.Context, keyID int64) error
func (s *Service) List(ctx context.Context, project string) ([]governance.KeyInfo, error)
```

`Authenticate` must parse, lookup by public ID, recompute and compare with `hmac.Equal`, check expiry/revocation against the injected clock, then persist `last_used_at`. Collapse only credential failures to `ErrInvalidCredential`; preserve database failures so middleware can return `503`.

For operator labels, use one private validator shared by create/list commands: trim surrounding whitespace, require 1–128 UTF-8 bytes, and reject Unicode control characters. SQL remains fully parameterized; validation is for stable identifiers and output safety, not SQL escaping.

`NewService` rejects a nil repository/random source/clock and peppers shorter than 32 bytes, then copies the pepper so caller mutation cannot change key verification. Map repository `ClientKey` values to digest-free `KeyInfo` before returning them from create/rotate/list.

In PostgreSQL, `CreateKey` must insert the project and key in one transaction using `INSERT ... ON CONFLICT (name) DO UPDATE SET name=EXCLUDED.name RETURNING id`. Key names are operator labels, not unique identifiers, so rotation may create a second row with the same name during overlap. `RotateKey` takes a row lock on the old key, inserts the replacement for the same project/name, then either stamps `revoked_at=now` or sets `expires_at` to the earlier of its existing expiry and `now+overlap`, all in one transaction. Any failure rolls back the replacement so the CLI never loses the only plaintext for an active key. HTTP authentication uses `KeyByPublicID` only and never creates projects.

- [ ] **Step 6: Run pure and database tests**

Run:

```bash
go test ./internal/domain/projectkey -count=1
go test ./internal/adapter/postgres -run 'TestProjectKey|TestKeyRotation' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/projectkey internal/adapter/postgres/keys.go internal/adapter/postgres/keys_test.go
git commit -m "Add secure project keys

[+] generate high-entropy one-time key values
[+] store keyed digests and project ownership
[+] support expiry revocation and overlap rotation"
```

---

### Task 4: Implement exact call admission and project budgets

**Files:**
- Create: `internal/domain/governance/budget/evaluator.go`
- Create: `internal/domain/governance/budget/evaluator_test.go`
- Create: `internal/adapter/postgres/admission.go`
- Create: `internal/adapter/postgres/admission_test.go`
- Create: `internal/adapter/postgres/budgets.go`
- Create: `internal/adapter/postgres/budgets_test.go`

**Interfaces:**
- Consumes: governance budget/request values from Task 2
- Produces: `budget.Evaluate(limits, totals) governance.Admission` from import path `internal/domain/governance/budget`
- Implements: `governance.RequestRepository` admission methods and `governance.BudgetRepository`
- Consumes later: middleware and budget CLI

- [ ] **Step 1: Write pure budget tests**

Use table tests for both rolling windows and both actions. The essential assertions are:

```go
tests := []struct {
	name string
	limit governance.BudgetLimit
	totals governance.WindowTotals
	blocked bool
}{
	{"call below cap", limit("calls", 5, "block"), totals(4, 0, 0), false},
	{"call at cap", limit("calls", 5, "block"), totals(5, 0, 0), true},
	{"tokens at cap", limit("tokens", 100, "block"), totals(0, 100, 0), true},
	{"cost at cap", limit("cost", 2.5, "block"), totals(0, 0, 2.5), true},
	{"unknown accounting blocks tokens", limit("tokens", 100, "block"), unknownAccounting(), true},
	{"unknown pricing blocks cost", limit("cost", 2.5, "block"), unknownPricing(), true},
	{"warning never blocks", limit("calls", 1, "warn"), totals(1, 0, 0), false},
}
```

Assert that calls ignore unknown token/cost accounting and that each breach returns the reset timestamp for its dimension/window.

- [ ] **Step 2: Run the budget tests and verify failure**

Run: `go test ./internal/domain/governance/budget -count=1`

Expected: FAIL because the new governance evaluator package does not exist.

- [ ] **Step 3: Implement the pure evaluator**

Implement:

```go
func Evaluate(
	limits []governance.BudgetLimit,
	totals map[governance.Window]governance.WindowTotals,
) governance.Admission
```

For the incoming generation request:

```text
calls blocks when recorded request count >= max
tokens blocks when persisted total tokens >= max
cost blocks when persisted priced cost >= max
tokens/cost block on accounting_unknown
cost additionally blocks on unknown_pricing
warn appends a warning but never flips Allowed
the first block in database ID order determines the 402 response
```

The request row itself is inserted only after `Evaluate` allows it; because prior generation rows already count, `calls >= max` is the exact pre-insert comparison.

- [ ] **Step 4: Write failing PostgreSQL admission tests**

Test with a real container:

```text
generation rows count calls whether in_flight or completed
metadata rows never count
token totals sum usage_attempt.total_tokens
cost totals sum only priced attempts
unknown accounting and unknown pricing flags are window-scoped
hour is now-60m and day is now-24h
each reset timestamp is the oldest still-contributing request/attempt timestamp plus its rolling-window duration
50 concurrent admissions under a calls max of 5 admit exactly 5
different projects do not share the advisory lock
blocked requests create no request_event
warned requests create one request_event and return warnings
SetBudget upserts one project/dimension/window/action row
SetBudget rejects NaN/infinity, negative values, and fractional call/token maxima
DeleteBudget and ListBudgets are project-safe
```

- [ ] **Step 5: Implement transactional admission**

`Store.AdmitGeneration` must:

```text
BEGIN
SELECT pg_advisory_xact_lock(project_id)
load project limits ORDER BY id
for each distinct window, aggregate calls from request_event and tokens/cost/unknown pricing from usage_attempt joined to generation requests
include accounting_unknown generation requests in the unknown flag
evaluate in Go
INSERT request_event when allowed
COMMIT
```

Populate `CallsResetAt` from the earliest counted generation request, `TokensResetAt` from the earliest token-bearing attempt or unknown-accounting request, and `CostResetAt` from the earliest priced attempt, unknown-priced attempt, or unknown-accounting request, each plus the rolling-window duration. If a zero-valued limit blocks with no contributing row, use `now` as its reset. Use UTC `now` supplied by the caller, not `time.Now()` inside domain logic.

`RecordMetadata` inserts `operation=metadata`, `state=in_flight`, and `accounting_state=not_applicable`. `CompleteRequest` updates only the named UUID, writes `completed_at`, state, and downstream status, and is idempotent.

- [ ] **Step 6: Run budget and admission tests**

Run:

```bash
go test ./internal/domain/governance/budget -count=1
go test ./internal/adapter/postgres -run 'TestAdmission|TestBudget' -count=1
```

Expected: PASS, including exactly five successful concurrent admissions.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/governance/budget internal/adapter/postgres/admission.go internal/adapter/postgres/admission_test.go internal/adapter/postgres/budgets.go internal/adapter/postgres/budgets_test.go
git commit -m "Enforce project budgets

[+] add rolling project-only budget evaluation
[+] serialize exact call admission in PostgreSQL
[+] fail closed on unknown token and cost accounting"
```

---

### Task 5: Build route policy, request context, middleware, and exclusive SDK access

**Files:**
- Create: `internal/adapter/cliproxy/routes.go`
- Create: `internal/adapter/cliproxy/routes_test.go`
- Create: `internal/adapter/cliproxy/context.go`
- Create: `internal/adapter/cliproxy/context_test.go`
- Create: `internal/adapter/cliproxy/access.go`
- Create: `internal/adapter/cliproxy/access_test.go`
- Create: `internal/adapter/cliproxy/middleware.go`
- Create: `internal/adapter/cliproxy/middleware_test.go`

**Interfaces:**
- Consumes: `projectkey.Service`, `governance.RequestRepository`
- Produces: `cliproxy.Classify(method, path) RouteClass`
- Produces: `cliproxy.WithIdentity`, `cliproxy.IdentityFromContext`
- Produces: `cliproxy.NewMiddleware(keys, requests, now) gin.HandlerFunc`
- Produces: SDK `access.Provider` implementation and exclusive-registration cleanup function
- Consumes later: embedded SDK service and usage plugin

- [ ] **Step 1: Write failing route-policy and middleware tests**

Use this exact policy table:

```go
tests := []struct{ method, path string; want RouteClass }{
	{"GET", "/healthz", RoutePublic},
	{"HEAD", "/healthz", RoutePublic},
	{"GET", "/", RouteDenied},
	{"GET", "/management.html", RouteDenied},
	{"GET", "/v0/management/config", RouteDenied},
	{"GET", "/v0/resource/plugins/x", RouteDenied},
	{"GET", "/anthropic/callback", RouteDenied},
	{"GET", "/codex/callback", RouteDenied},
	{"GET", "/antigravity/callback", RouteDenied},
	{"GET", "/v1/models", RouteMetadata},
	{"GET", "/v1beta/models", RouteMetadata},
	{"GET", "/v1beta/models/gemini-test", RouteMetadata},
	{"POST", "/v1/messages/count_tokens", RouteMetadata},
	{"POST", "/v1beta/models/gemini-test:countTokens", RouteMetadata},
	{"POST", "/v1/messages", RouteGeneration},
	{"POST", "/v1/responses", RouteGeneration},
	{"GET", "/new-sdk-route", RouteGeneration},
}
```

With a Gin test recorder and LLMGW fakes only, assert the middleware returns `404` for denied routes without calling dependencies, indistinguishable `401` bodies for missing/malformed/unknown credentials, `503` for authenticator/repository failures, and stable `402 budget_exceeded` for a blocking admission. Assert allowed requests pass immutable identity to the next handler and call `CompleteRequest` with its final status; metadata calls `RecordMetadata`, while generation calls `AdmitGeneration`.

- [ ] **Step 2: Run policy tests and verify failure**

Run: `go test ./internal/adapter/cliproxy -run 'TestClassify|TestIdentity|TestAccess|TestMiddleware' -count=1`

Expected: FAIL because the adapter package does not exist.

- [ ] **Step 3: Implement route and context primitives**

Define:

```go
type RouteClass uint8
const (
	RouteGeneration RouteClass = iota
	RoutePublic
	RouteDenied
	RouteMetadata
)

type RequestIdentity struct {
	RequestID string
	ProjectID int64
	ClientKeyID int64
	KeyPublicID string
	Operation governance.Operation
}

func WithIdentity(ctx context.Context, identity RequestIdentity) context.Context
func IdentityFromContext(ctx context.Context) (RequestIdentity, bool)
```

Keep the context key unexported and store the value, not a mutable pointer.

- [ ] **Step 4: Implement header parsing and the SDK access bridge**

Create a pure helper:

```go
func credential(headers http.Header) (string, error)
```

It accepts one Bearer authorization value or one `x-api-key`, rejects every other scheme, rejects duplicates, and requires equality when both fields are present. Its errors never contain the credential.

Implement:

```go
const AccessProviderType = "llmgw-project-key"

type AccessProvider struct{}

func (AccessProvider) Identifier() string
func (AccessProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError)
func RegisterExclusiveAccess(provider sdkaccess.Provider) func()
```

The provider accepts only an identity already present in request context and returns:

```go
&sdkaccess.Result{
	Provider: AccessProviderType,
	Principal: identity.KeyPublicID,
	Metadata: map[string]string{"request_id": identity.RequestID},
}
```

`RegisterExclusiveAccess` registers the provider, calls `SetExclusiveProvider`, and returns a cleanup that clears exclusivity and unregisters only `AccessProviderType`.

- [ ] **Step 5: Implement the middleware**

Use local interfaces so tests can substitute LLMGW behavior without reimplementing CLIProxy:

```go
type KeyAuthenticator interface {
	Authenticate(context.Context, string) (governance.KeyIdentity, error)
}

func NewMiddleware(
	keys KeyAuthenticator,
	requests governance.RequestRepository,
	now func() time.Time,
) gin.HandlerFunc
```

Per request:

```text
RouteDenied: abort 404 before auth.
RoutePublic: continue without identity.
Metadata/generation: parse and authenticate key; credential errors return generic 401.
Repository/auth infrastructure errors return 503.
Generate UUID, attach immutable identity to c.Request.Context().
Metadata: insert an unmetered request row.
Generation: call atomic admission; a block returns 402 budget_exceeded with dimension/window/reset_at only.
Continue into the SDK.
After c.Next(), complete the request with c.Writer.Status() using context.WithoutCancel, a five-second timeout, and bounded retry.
Log warnings using project ID, never project key.
```

Do not parse or buffer request bodies; the usage plugin fills `requested_model` from the SDK alias.

- [ ] **Step 6: Verify adapter primitives**

Run:

```bash
gofmt -w internal/adapter/cliproxy
go test ./internal/adapter/cliproxy -run 'TestClassify|TestCredential|TestIdentity|TestAccess|TestMiddleware' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/adapter/cliproxy
git commit -m "Guard embedded proxy requests

[+] classify public metadata and generation routes
[+] authenticate project keys before SDK route auth
[!] hard-deny management and server OAuth callbacks"
```

---

### Task 6: Embed the real CLIProxyAPI service and establish the integration harness

**Files:**
- Create: `internal/adapter/cliproxy/service.go`
- Create: `internal/adapter/cliproxy/service_test.go`
- Create: `test/integration/main_test.go`
- Create: `test/integration/harness_test.go`
- Create: `test/integration/stub_upstream_test.go`
- Create: `test/integration/auth_routes_test.go`

**Interfaces:**
- Consumes: Task 1 config, Task 5 middleware/access provider
- Produces: `cliproxy.NewService(cfg, middleware, usagePlugins...)`
- Produces: `Service.Run(context.Context) error`
- Produces: one-process integration harness shared by all embedded SDK tests
- Consumes later: usage plugin and composition root

- [ ] **Step 1: Write a failing service-construction test**

Assert that construction:

```text
uses sdk/cliproxy.NewBuilder with WithConfig and WithConfigPath
passes sdk/api.WithMiddleware(governanceMiddleware)
passes sdk/api.WithRequestLoggerFactory returning nil
uses a caller-supplied sdk/access.Manager
registers the exclusive LLMGW access provider before Build
registers each supplied sdk usage.Plugin
returns build errors with cliproxy context
```

Expose only:

```go
type Service struct {
	proxy *sdkproxy.Service
	clearAccess func()
}

func NewService(cfg config.Config, middleware gin.HandlerFunc, plugins ...sdkusage.Plugin) (*Service, error)
func (s *Service) Run(ctx context.Context) error
```

- [ ] **Step 2: Run the service test and verify failure**

Run: `go test ./internal/adapter/cliproxy -run TestNewService -count=1`

Expected: FAIL because `service.go` does not exist.

- [ ] **Step 3: Implement the SDK wrapper**

Build with:

```go
manager := sdkaccess.NewManager()
clear := RegisterExclusiveAccess(AccessProvider{})
proxy, err := sdkproxy.NewBuilder().
	WithConfig(cfg.Proxy).
	WithConfigPath(cfg.Path).
	WithRequestAccessManager(manager).
	WithServerOptions(
		sdkapi.WithMiddleware(middleware),
		sdkapi.WithRequestLoggerFactory(func(*sdkconfig.Config, string) sdklogging.RequestLogger {
			return nil
		}),
	).
	Build()
```

Register plugins after `Build`. On build failure, run `clear`. `Run` delegates to `proxy.Run`, treats `context.Canceled` as normal shutdown, and clears the global access registration only after SDK shutdown has drained usage.

- [ ] **Step 4: Build one integration harness around the real SDK**

`TestMain` must start exactly one SDK service because the pinned SDK's default usage manager is global and cannot restart after `StopDefault`. The harness owns:

```go
type Harness struct {
	BaseURL string
	ConfigPath string
	AuthDir string
	Store *postgres.Store
	Keys *projectkey.Service
	Upstream *StubUpstream
	cancel context.CancelFunc
	done <-chan error
}
```

Start one PostgreSQL 16 container, one `httptest.Server` upstream, choose one free local port, write a temporary config, build the embedded service, and poll `GET /healthz`.

Configure the actual native OpenAI-compatible provider with the concrete `httptest.Server` URL:

```go
proxyYAML := fmt.Sprintf(`
openai-compatibility:
  - name: integration
    base-url: %s/v1
    api-key-entries:
      - api-key: upstream-account-a
      - api-key: upstream-account-b
    models:
      - name: upstream-model
        alias: test-model
        force-mapping: true
`, upstream.URL)
```

The stub must support scripted status, headers, JSON/SSE body, and capture of selected upstream authorization without printing it.

- [ ] **Step 5: Add real-SDK auth and route tests**

Through HTTP, assert:

```text
GET /healthz returns 200 without a key.
POST /v1/chat/completions without a key returns 401.
A generated key works in Authorization Bearer.
The same key works in x-api-key.
Mismatched dual headers return the same 401 as an unknown key.
GET /v1/models is authenticated but creates metadata, not generation, accounting.
GET /, all callback paths, /management.html, /v0/management/config, and /v0/resource/plugins/x return 404 even with a valid key.
A random new route requires auth and consumes one generation call before the SDK returns 404.
The SDK usage principal is the public key ID, never the raw key.
```

- [ ] **Step 6: Run the embedded integration tests**

Run:

```bash
go test ./internal/adapter/cliproxy -count=1
go test ./test/integration -run 'TestHealth|TestProjectAuth|TestRoutePolicy' -count=1 -v
```

Expected: PASS with a real SDK service and no live provider calls.

- [ ] **Step 7: Commit**

```bash
git add internal/adapter/cliproxy/service.go internal/adapter/cliproxy/service_test.go test/integration
git commit -m "Embed CLIProxyAPI service

[+] build and run the pinned SDK in process
[+] disable SDK request body logging
[+] add a real-SDK hermetic integration harness"
```

---

### Task 7: Persist normalized usage attempts and notional cost

**Files:**
- Create: `internal/domain/governance/cost/calculator.go`
- Create: `internal/domain/governance/cost/calculator_test.go`
- Create: `internal/adapter/postgres/attempts.go`
- Create: `internal/adapter/postgres/attempts_test.go`
- Create: `internal/adapter/cliproxy/usage.go`
- Create: `internal/adapter/cliproxy/usage_test.go`
- Create: `test/integration/usage_test.go`

**Interfaces:**
- Consumes: SDK `usage.Record`, Task 5 request identity, governance `UsageRepository`
- Produces: `cost.Calculate(tokens, rule) (float64, bool)` from import path `internal/domain/governance/cost`
- Produces: `cliproxy.NewUsagePlugin(repo) sdkusage.Plugin`
- Implements: price resolution and attempt persistence

- [ ] **Step 1: Write pure cost tests**

Test:

```go
tokens := governance.TokenBreakdown{
	Total: 190,
	UncachedInput: 100,
	CacheRead: 20,
	CacheCreation: 10,
	Output: 60,
	Reasoning: 15,
}
rule := governance.PriceRule{
	InputPerMillion: ptr(3.0),
	OutputPerMillion: ptr(15.0),
	CacheReadPerMillion: ptr(0.3),
	CacheCreationPerMillion: ptr(3.75),
}
```

Expected cost is `(100*3 + 20*0.3 + 10*3.75 + 60*15) / 1_000_000`; reasoning is not added beyond the 60 output tokens. Return `known=false` when a used bucket has a nil rate, when token quality is inconsistent/unclassified, when a rate/result is NaN or infinite, or when no price rule exists.

- [ ] **Step 2: Write SDK-record mapping tests**

Construct `sdkusage.Record` values and assert the mapper:

```text
uses EnsureTokenBreakdownForProvider
copies provider, executor type, model, alias, AuthID/AuthType, tiers, latency, TTFT, failure status, and requested time
drops APIKey, Source, Failure.Body, and ResponseHeaders
uses request ID/project/key only from RequestIdentity
maps canonical non-overlapping token buckets
generates one stable attempt UUID before persistence retries
```

- [ ] **Step 3: Run pure tests and verify failure**

Run:

```bash
go test ./internal/domain/governance/cost -count=1
go test ./internal/adapter/cliproxy -run TestUsageRecordMapping -count=1
```

Expected: FAIL because canonical pricing and the mapper are not implemented.

- [ ] **Step 4: Write failing price-rule and attempt-persistence tests**

With `newGovernanceStore`, assert:

```text
exact provider beats provider '*'
exact service tier beats tier '*'
the pattern with the most literal bytes wins
the latest effective_from not after RequestedAt wins
a future rule is ignored
no matching rule returns (zero, false, nil)
RecordAttempt inserts one row and changes its generation parent to observed
the same attempt UUID is idempotent
two different attempts attach to one request
metadata attempts do not change not_applicable
resolved_zero/accounting_unknown becomes observed when real late usage arrives
```

Run: `go test ./internal/adapter/postgres -run 'TestPriceRule|TestRecordAttempt' -count=1`

Expected: FAIL because price resolution and attempt persistence are missing.

- [ ] **Step 5: Implement price lookup and attempt persistence**

`PriceRuleFor` must load rules effective at `record.RequestedAt`, consider exact provider/tier before `*`, match model patterns where `*` means any byte sequence, prefer the most literal/specific match, then prefer latest `effective_from`.

`RecordAttempt` must insert by attempt UUID and update its parent generation request in the same transaction:

```sql
UPDATE request_event
SET accounting_state = 'observed',
    requested_model = COALESCE(requested_model, $alias),
    accounting_resolved_at = NULL
WHERE id = $request_id AND operation = 'generation';
```

For metadata requests, persist the attempt for audit but leave `accounting_state=not_applicable`. Use `ON CONFLICT (id) DO NOTHING` so a database retry cannot duplicate an attempt.

- [ ] **Step 6: Implement the usage plugin**

Implement:

```go
type UsagePlugin struct {
	repo governance.UsageRepository
	now func() time.Time
}

func NewUsagePlugin(repo governance.UsageRepository) *UsagePlugin
func (p *UsagePlugin) HandleUsage(ctx context.Context, record sdkusage.Record)
```

`HandleUsage` must:

```text
recover and log its own panic without values from record
read immutable request identity from ctx; log and discard if missing
generate the attempt ID once
detach cancellation with context.WithoutCancel and add a 5-second deadline
resolve pricing, calculate cost, and persist synchronously inside the callback
retry transient repository errors at 50ms then 100ms with the same attempt ID
store null cost and unknown_pricing instead of returning an error for missing prices
```

- [ ] **Step 7: Add real-SDK usage assertions**

Script the upstream for:

```text
one successful non-stream response with 10 input and 4 output tokens
one failed account attempt followed by a success
one streaming response with final usage
one response using an unpriced model
```

Wait by polling PostgreSQL, not sleeping a fixed duration. Assert one `request_event` per client request, one `usage_attempt` per SDK attempt, calls counted once, attempt tokens summed, public key ID present only via the foreign key, and unknown pricing causes a null cost.

- [ ] **Step 8: Run usage tests**

Run:

```bash
go test ./internal/domain/governance/cost -count=1
go test ./internal/adapter/cliproxy -run 'TestUsage' -count=1
go test ./internal/adapter/postgres -run 'TestPriceRule|TestRecordAttempt' -count=1
go test ./test/integration -run 'TestUsage|TestRetryAttempts|TestStreamingUsage' -count=1 -v
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/domain/governance/cost internal/adapter/postgres/attempts.go internal/adapter/postgres/attempts_test.go internal/adapter/cliproxy/usage.go internal/adapter/cliproxy/usage_test.go test/integration/usage_test.go
git commit -m "Persist SDK usage attempts

[+] map canonical SDK token accounting
[+] record every retry and failover attempt
[+] calculate provider-aware notional cost"
```

---

### Task 8: Reconcile incomplete accounting and retain bounded history

**Files:**
- Create: `internal/adapter/postgres/reconcile.go`
- Create: `internal/adapter/postgres/reconcile_test.go`
- Create: `internal/adapter/postgres/governance_retention.go`
- Create: `internal/adapter/postgres/governance_retention_test.go`
- Create: `internal/command/workers.go`
- Create: `internal/command/workers_test.go`

**Interfaces:**
- Implements: remaining `governance.UsageRepository` lifecycle methods
- Produces: `command.StartWorkers(ctx, repo, retention) <-chan struct{}`
- Consumes later: `serve` composition and `usage resolve`

- [ ] **Step 1: Write failing reconciliation tests**

With a real database, assert:

```text
RecoverInterrupted marks every pre-start in_flight generation request completed+accounting_unknown.
RecoverInterrupted marks interrupted metadata completed+not_applicable.
A completed pending request older than 30s with attempts becomes observed.
A completed pending request older than 30s without attempts becomes accounting_unknown.
A late attempt changes accounting_unknown or resolved_zero to observed.
An in-flight row older than 6h becomes completed+accounting_unknown.
ResolveUnknownAsZero changes only accounting_unknown to resolved_zero and stamps accounting_resolved_at.
ResolveUnknownAsZero leaves the request counted as one call.
ResolveUnknownAsZero rejects pending, observed, metadata, and unknown UUIDs.
```

- [ ] **Step 2: Run reconciliation tests and verify failure**

Run: `go test ./internal/adapter/postgres -run 'TestRecover|TestReconcile|TestResolveUnknown' -count=1`

Expected: FAIL because lifecycle methods are missing.

- [ ] **Step 3: Implement recovery and reconciliation**

Use these constants in `workers.go`:

```go
const (
	reconcileInterval = 5 * time.Second
	settlementDelay = 30 * time.Second
	staleInFlightAge = 6 * time.Hour
	pruneInterval = time.Hour
)
```

All transitions must use guarded SQL predicates so plugin writes and the reconciler can race safely. `RecordAttempt` wins over unknown/resolved-zero whenever real usage arrives.

Run `RecoverInterrupted` once before constructing/listening with the SDK. During runtime, call `ReconcileAccounting` immediately and every five seconds.

- [ ] **Step 4: Write governance retention tests**

Assert:

```text
completed request_event older than retention is deleted with usage_attempt cascade
recent completed and every in-flight request remain
client_key, project, budget, prices, legacy_usage_event, and legacy_budget_limit are never pruned
```

- [ ] **Step 5: Implement retention and worker shutdown**

`PruneCompletedRequests` deletes only:

```sql
DELETE FROM request_event
WHERE state = 'completed'
  AND requested_at < now() - make_interval(secs => $1)
```

`StartWorkers` starts one goroutine with separate reconcile/prune tickers, logs counts without request payloads, exits on context cancellation, and closes the returned channel. The database remains open until the SDK has drained usage and the worker channel has closed.

- [ ] **Step 6: Run lifecycle tests**

Run:

```bash
go test ./internal/adapter/postgres -run 'TestRecover|TestReconcile|TestResolveUnknown|TestPrune' -count=1
go test ./internal/command -run TestWorkers -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/adapter/postgres/reconcile.go internal/adapter/postgres/reconcile_test.go internal/adapter/postgres/governance_retention.go internal/adapter/postgres/governance_retention_test.go internal/command/workers.go internal/command/workers_test.go
git commit -m "Recover incomplete accounting

[+] reconcile asynchronous usage after request completion
[+] support explicit assume-zero resolution
[+] prune completed request trees without touching legacy data"
```

---

### Task 9: Add local key, budget, and usage commands

**Files:**
- Create: `internal/command/config.go`
- Create: `internal/command/config_test.go`
- Create: `internal/command/key.go`
- Create: `internal/command/key_test.go`
- Create: `internal/command/budget.go`
- Create: `internal/command/budget_test.go`
- Create: `internal/command/usage.go`
- Create: `internal/command/usage_test.go`
- Create: `internal/adapter/postgres/reporting.go`
- Create: `internal/adapter/postgres/reporting_test.go`

**Interfaces:**
- Consumes: config, project-key service, budget repository, usage repository
- Produces: `command.Streams`, shared config/store opening, and the `runKey`, `runBudget`, and `runUsage` leaf handlers
- Produces: every non-auth administrative command from the spec
- Consumes later: Task 12 root dispatch

- [ ] **Step 1: Write failing shared-option and leaf-parser tests**

Define:

```go
type Streams struct {
	In io.Reader
	Out io.Writer
	Err io.Writer
	Getenv func(string) string
	ConfigPath string
}
```

Test:

```text
unknown nested commands return usage errors
an empty Streams.ConfigPath and LLMGW_CONFIG defaults to ./config.yaml
LLMGW_CONFIG supplies the path when Streams.ConfigPath is empty
Streams.ConfigPath overrides LLMGW_CONFIG for key, budget, and usage
no command starts or calls a remote management API
```

- [ ] **Step 2: Write persistence-backed CLI tests**

Against ephemeral PostgreSQL, drive:

```text
key create analytics --name server-1
key list analytics
key list
key create analytics --name ephemeral --expires 24h
key rotate KEY_ID --overlap 24h
key revoke KEY_ID
budget set analytics --dimension calls --window hour --max 50 --action block
budget list analytics
budget list
budget delete LIMIT_ID
usage show analytics --since 24h --by key
usage show analytics --since 24h --by model
usage show analytics --since 24h --by provider
usage resolve REQUEST_UUID --assume-zero
```

Assert key plaintext appears only in create/rotate output; list output contains names, IDs, expiry/revocation, never digest.

- [ ] **Step 3: Run leaf-command tests and verify failure**

Run: `go test ./internal/command -run 'TestCommandConfig|TestKeyCommand|TestBudgetCommand|TestUsageCommand' -count=1`

Expected: FAIL because commands are missing.

- [ ] **Step 4: Implement standard-library command parsing**

Use `flag.NewFlagSet` per leaf command with `flag.ContinueOnError` and direct help output to `Streams.Err`. The shared `--config` option is parsed once by Task 12's root before dispatch, then placed in `Streams.ConfigPath`; leaf commands do not each redefine it. Implement these handlers:

```go
func runKey(ctx context.Context, args []string, streams Streams) error
func runBudget(ctx context.Context, args []string, streams Streams) error
func runUsage(ctx context.Context, args []string, streams Streams) error
```

Keep database opening in one helper:

```go
func openStore(ctx context.Context, path string, streams Streams) (config.Config, *postgres.Store, error)
```

Do not load the key pepper for budget/usage commands. Load it only for key create/rotate and `serve`.

`key create` creates a missing project. Every other key/budget/usage command requires an existing target and returns a contextual error.

- [ ] **Step 5: Implement reporting queries**

Map `--by` through a fixed whitelist:

```go
var groupColumns = map[string]string{
	"key": "client_key.name",
	"model": "COALESCE(usage_attempt.resolved_model, request_event.requested_model, '')",
	"provider": "usage_attempt.provider",
}
```

Never interpolate raw user input. Reports must show calls from `request_event`, token/cost totals from attempts, failed attempt count, unknown accounting count, and unknown pricing count. Do not print upstream auth IDs unless a future explicit diagnostic command is designed.

- [ ] **Step 6: Implement explicit resolution**

Require the literal `--assume-zero` flag; without it, return a usage error and make no change. Print request ID, old state, `resolved_zero`, and timestamp, but no request body or provider credential.

The repository transition is a single guarded statement:

```sql
UPDATE request_event
SET accounting_state = 'resolved_zero',
    accounting_resolved_at = $2
WHERE id = $1
  AND operation = 'generation'
  AND accounting_state = 'accounting_unknown'
RETURNING id;
```

- [ ] **Step 7: Run CLI tests**

Run:

```bash
go test ./internal/command -run 'TestCommandConfig|TestKeyCommand|TestBudgetCommand|TestUsageCommand' -count=1
go test ./internal/adapter/postgres -run TestUsageReporting -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/command/config.go internal/command/config_test.go internal/command/key.go internal/command/key_test.go internal/command/budget.go internal/command/budget_test.go internal/command/usage.go internal/command/usage_test.go internal/adapter/postgres/reporting.go internal/adapter/postgres/reporting_test.go
git commit -m "Add local governance commands

[+] create rotate list and revoke project keys
[+] manage project budgets from the binary
[+] inspect and resolve usage accounting locally"
```

---

### Task 10: Add provider OAuth, auth listing, and legacy credential import

**Files:**
- Create: `internal/adapter/cliproxy/auth.go`
- Create: `internal/adapter/cliproxy/auth_test.go`
- Create: `internal/adapter/postgres/legacy_auth.go`
- Create: `internal/adapter/postgres/legacy_auth_test.go`
- Create: `internal/command/auth.go`
- Create: `internal/command/auth_test.go`

**Interfaces:**
- Consumes: public `sdk/auth` authenticators and file token store
- Produces: `runAuth(ctx context.Context, args []string, streams Streams) error`
- Produces: `cliproxy.PrepareAuthDir(path string) error`
- Produces: `auth login <provider>`, `auth list`, `auth import-legacy`
- Implements: read-only legacy credential repository

- [ ] **Step 1: Write failing auth-list/import tests**

Test with a temporary auth directory and legacy database rows:

```text
auth list prints ID/provider/label/disabled only
Claude row with refresh token imports to a type=claude auth JSON
Codex row with refresh token and chatgpt_account_id imports to type=codex
rows lacking required fields are reported as needs-login
session_key is never written to CLIProxy auth JSON
second import is idempotent and does not overwrite an existing file
source oauth_token rows remain unchanged
file mode is 0600 and directory mode is 0700
stdout/stderr never contain access token, refresh token, session key, or account ID
```

- [ ] **Step 2: Run auth tests and verify failure**

Run:

```bash
go test ./internal/adapter/cliproxy -run 'TestAuthList|TestImportLegacy' -count=1
go test ./internal/command -run TestAuthCommand -count=1
```

Expected: FAIL because auth helpers and commands are missing.

- [ ] **Step 3: Implement the public SDK auth manager**

Construct:

```go
store := sdkauth.NewFileTokenStore()
store.SetBaseDir(cfg.Proxy.AuthDir)
manager := sdkauth.NewManager(
	store,
	sdkauth.NewCodexAuthenticator(),
	sdkauth.NewClaudeAuthenticator(),
	sdkauth.NewAntigravityAuthenticator(),
	sdkauth.NewKimiAuthenticator(),
	sdkauth.NewXAIAuthenticator(),
)
```

`auth login` accepts only `claude|codex|antigravity|kimi|xai`, plus `--no-browser`, `--callback-port`, and `--device` for Codex. Pass a line-reading prompt function through `sdkauth.LoginOptions`; `--device` sets metadata `codex_login_mode=device`. Print the saved path and safe provider/label only.

Before login, list, import, or serve, call `PrepareAuthDir`: reject an empty path or symlink, create the directory with `0700`, require it to be a directory, and enforce `0700`. Return a contextual error before the SDK starts if any check fails.

- [ ] **Step 4: Implement legacy export without importing upstream internals**

Query `oauth_token JOIN provider` into `governance.LegacyCredential`. Save through the public `FileTokenStore` using `coreauth.Auth.Metadata`.

Claude metadata:

```go
map[string]any{
	"type": "claude",
	"email": legacy.AccountLabel,
	"access_token": legacy.AccessToken,
	"refresh_token": legacy.RefreshToken,
}
```

Add `"expired": legacy.ExpiresAt.Format(time.RFC3339)` only when `ExpiresAt != nil`. Codex metadata adds `"account_id": legacy.ChatGPTAccountID` and uses `"type": "codex"`. Set `coreauth.Auth.FileName` to the sanitized deterministic filename `claude-legacy-<label>.json` or `codex-legacy-<label>.json` before calling `FileTokenStore.Save`. Create/chmod the auth directory to `0700`, reject labels that sanitize to an empty basename, and use `os.Lstat` to skip any existing file or symlink instead of letting `Save` overwrite it. The private directory makes the `Lstat`/save sequence safe from unprivileged local races; the saved file must be `0600`. Never import a Claude session key because CLIProxyAPI does not consume that format.

- [ ] **Step 5: Wire and verify auth commands**

`runAuth` always loads the shared YAML. `auth login` and `auth list` use only `cfg.Proxy.AuthDir` and never resolve PostgreSQL or pepper secrets. `auth import-legacy` resolves `DatabaseDSN`, opens PostgreSQL, imports, and closes it; it never resolves the key pepper.

Run:

```bash
go test ./internal/adapter/cliproxy -run 'TestAuthList|TestImportLegacy' -count=1
go test ./internal/adapter/postgres -run TestLegacyCredentials -count=1
go test ./internal/command -run TestAuthCommand -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/adapter/cliproxy/auth.go internal/adapter/cliproxy/auth_test.go internal/adapter/postgres/legacy_auth.go internal/adapter/postgres/legacy_auth_test.go internal/command/auth.go internal/command/auth_test.go
git commit -m "Wrap provider authentication

[+] expose SDK OAuth login and safe auth listing
[+] import compatible legacy Claude and Codex tokens
[!] require re-login when legacy credentials are incomplete"
```

---

### Task 11: Complete protocol, budget, failure, and account-pool integration coverage

**Files:**
- Create: `test/integration/protocols_test.go`
- Create: `test/integration/budgets_test.go`
- Create: `test/integration/account_pool_test.go`
- Create: `test/integration/failures_test.go`
- Modify: `test/integration/harness_test.go`
- Modify: `test/integration/stub_upstream_test.go`

**Interfaces:**
- Consumes: the real embedded service and deterministic native OpenAI-compatible upstream from Task 6
- Produces: the release-gating hermetic suite

- [ ] **Step 1: Add protocol parity cases**

Drive and assert valid downstream response shape for:

```text
Anthropic POST /v1/messages, streaming and non-streaming
OpenAI POST /v1/chat/completions, streaming and non-streaming
OpenAI POST /v1/responses, streaming and non-streaming
Gemini POST /v1beta/models/test-model:generateContent
Anthropic POST /v1/messages/count_tokens, authenticated and unmetered
GET /v1/models and GET /v1beta/models, authenticated and unmetered
client cancellation before first chunk and after first chunk
malformed JSON counted once with no upstream retry
an expired key and a revoked key receive the same generic 401 as an unknown key
conflicting Authorization/x-api-key values receive that same generic 401
a valid key plus forged X-Project/X-Tags values remains attributed to the key's project
```

Use protocol-valid fixture bodies in the stub and assert structure/status, never generated text equality.

- [ ] **Step 2: Add budget concurrency cases**

Assert through HTTP and PostgreSQL:

```text
calls max 5 admits exactly 5 of 50 concurrent requests
one failed upstream attempt plus one success consumes one call
tokens sum all attempts and block only the next request after crossing
cost sums all priced attempts and blocks only the next request after crossing
concurrent accepted requests may overshoot tokens/cost
unknown pricing blocks only projects with an active blocking cost limit
accounting_unknown blocks token/cost but not calls-only projects
warn limits never change SDK response
database unavailable during auth/admission returns 503 without contacting upstream
temporarily revoking INSERT on usage_attempt makes usage persistence fail without altering the completed client response
after the settlement delay, missing usage becomes accounting_unknown and blocks only active token/cost block limits
restoring the INSERT grant lets new attempts persist without restarting the proxy
```

- [ ] **Step 3: Add account-pool chaos cases**

Script account A/B authorization values without logging them:

```text
429 with reset metadata cools only the affected account/model and fails over
429 without reset metadata follows pinned SDK behavior and never creates an LLMGW blacklist
503 fails over but transient-error-cooldown-seconds=-1 avoids a 60-second quarantine
network close before headers fails over
all accounts cooling returns the SDK status and Retry-After
a newly added auth file becomes eligible through hot reload
stream failure before first payload may fail over
stream failure after first payload does not replay a second response
```

Assert no LLMGW table stores account cooldowns; account eligibility remains SDK-owned.

Capture test logs in memory and assert they contain none of the raw project key, upstream authorization values, fixture prompt/tool payload, `usage.Record.Source`, upstream failure body, or response headers.

- [ ] **Step 4: Test hostile config reloads**

Rewrite the watched config to add:

```yaml
api-keys: [native-bypass]
remote-management:
  secret-key: attempted-management-key
  disable-control-panel: false
```

After the SDK reloads, assert `native-bypass` receives `401`, LLMGW keys still work, and every management/control-panel path remains `404`. Restore the valid config and assert service continuity.

- [ ] **Step 5: Run the complete hermetic suite**

Run:

```bash
go test ./test/integration -count=1 -v
go test -race ./internal/config ./internal/domain/governance/... ./internal/domain/projectkey ./internal/adapter/cliproxy ./internal/command -count=1
go test -race ./internal/adapter/postgres -run 'Test(CLIProxyGovernanceMigration|ProjectKey|KeyRotation|Admission|Budget|PriceRule|RecordAttempt|Recover|Reconcile|ResolveUnknown|Prune|UsageReporting|LegacyCredentials)' -count=1
```

Expected: PASS with no external provider traffic. Do not run the legacy native-adapter tests against migration `0010`; Task 12 removes that stopped-traffic runtime and its tests before the repository-wide gate.

- [ ] **Step 6: Commit**

```bash
git add test/integration
git commit -m "Gate embedded proxy behavior

[+] cover Anthropic OpenAI Responses and Gemini protocols
[+] verify budget and account-pool failure semantics
[!] preserve exclusive access across hostile config reloads"
```

---

### Task 12: Cut over the composition root and remove the native proxy stack

**Files:**
- Rewrite: `cmd/llmgw/main.go`
- Create: `internal/command/root.go`
- Create: `internal/command/root_test.go`
- Create: `internal/command/serve.go`
- Create: `internal/command/serve_test.go`
- Create: `test/integration/lifecycle_subprocess_test.go`
- Delete: `internal/config/legacy.go`
- Delete: `internal/adapter/httpserver/`
- Delete: `internal/adapter/provider/`
- Delete: `internal/domain/budget/`
- Delete: `internal/domain/llm/`
- Delete: `internal/domain/usage/`
- Delete: `internal/domain/errors.go`
- Delete: `internal/domain/ports.go`
- Delete: `internal/adapter/postgres/accounts.go`
- Delete: `internal/adapter/postgres/budget.go`
- Delete: `internal/adapter/postgres/budget_test.go`
- Delete: `internal/adapter/postgres/retention.go`
- Delete: `internal/adapter/postgres/retention_test.go`
- Delete: `internal/adapter/postgres/usage.go`
- Delete: `internal/adapter/postgres/store_test.go`
- Modify: `internal/adapter/postgres/store.go`
- Create: `internal/adapter/postgres/store_lifecycle_test.go`
- Replace: `test/e2e/`

**Interfaces:**
- Consumes: every prior task
- Produces: `command.Run(ctx context.Context, args []string, streams command.Streams) error`
- Produces: the final `llmgw [serve]` runtime
- Preserves: read-only legacy credentials only through `legacy_auth.go`

- [ ] **Step 1: Write failing root and serve lifecycle tests**

Build a temporary YAML and assert:

```text
serve fails before listening when PostgreSQL is unavailable
serve fails before listening when pepper/config/auth-dir validation fails
serve calls RecoverInterrupted before SDK construction
serve starts workers and embedded SDK on success
root cancellation waits for SDK usage drain, then workers, then PostgreSQL close
an unexpected SDK return is propagated so the process manager restarts the unit
running llmgw with no args is equivalent to llmgw serve
llmgw serve is accepted explicitly
auth, key, budget, and usage dispatch to their leaf handlers
unknown top-level commands return a usage error
--config before the command overrides LLMGW_CONFIG for every branch
--config after the command is rejected as a leaf usage error
--help lists serve, auth, key, budget, and usage
```

- [ ] **Step 2: Run serve tests and verify failure**

Run: `go test ./internal/command -run 'TestRoot|TestServe' -count=1`

Expected: FAIL because the composition root still builds native Claude/Codex providers.

- [ ] **Step 3: Implement root dispatch and the final serve composition**

`root.go` defines:

```go
func Run(ctx context.Context, args []string, streams Streams) error
```

Use a root `flag.FlagSet` for `--config` and `--help`; global flags must precede the command, for example `llmgw --config /etc/llmgw/config.yaml key list`. Put the parsed path in `Streams.ConfigPath`. No command arguments dispatch to `serve`; explicit `serve`, `auth`, `key`, `budget`, and `usage` dispatch to their handlers. Resolve `Streams.ConfigPath`, then `LLMGW_CONFIG`, then `./config.yaml` before opening command-specific dependencies, and return a usage error for unknown commands.

The sequence in `serve.go` must be:

```text
load and validate YAML/secrets
open PostgreSQL and ping
recover interrupted requests
construct project-key service
construct governance middleware
construct usage plugin
construct embedded CLIProxy service
start reconcile/retention workers
run SDK service with signal context
wait for SDK Run to return and drain usage
cancel/wait workers
close PostgreSQL
```

`cmd/llmgw/main.go` becomes:

```go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := command.Run(ctx, os.Args[1:], command.Streams{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Getenv: os.Getenv,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "llmgw: %v\n", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Verify graceful shutdown in an isolated subprocess**

Build `./cmd/llmgw`, start it with the test PostgreSQL/stub configuration, issue a streaming request whose final usage is queued, send `SIGTERM`, and assert:

```text
process exits within 30 seconds
client request row remains
final queued usage attempt is persisted before exit
no request is left in_flight after middleware completion
```

Use a subprocess because the SDK global usage manager intentionally stops once per process.

Run: `go test ./test/integration -run TestGracefulShutdownSubprocess -count=1 -v`

Expected: PASS.

- [ ] **Step 5: Delete the old runtime only after the new serve and shutdown tests pass**

Remove native HTTP parsing/streaming, Claude Max, Codex, old request/usage types, the temporary `LoadLegacy` bridge, reservation logic, tag logic, provider pool logic, and their tests. Keep historical migrations `0001` through `0009`, archived tables, and the read-only legacy auth query.

Run:

```bash
go mod tidy
rg 'X-Project|X-Tags|LLMGW_DEFAULT_PROJECT|WholeProjectTag|ReserveIfAdmitted' --glob '*.go'
rg '^replace ' go.mod
```

Expected: no runtime Go matches for removed governance concepts and no `replace`.

- [ ] **Step 6: Replace gated live tests**

The new `test/e2e` suite is enabled only when `LLMGW_LIVE_CONFIG` points to an operator-owned YAML/auth directory. It must:

```text
create a temporary project key in the configured test database
perform one non-stream and one stream request for each configured Claude/Codex protocol
assert normalized request/attempt rows
assert failover only when at least two safe test accounts exist
never print prompts, keys, auth IDs, or provider tokens
skip cleanly when LLMGW_LIVE_CONFIG is absent
```

- [ ] **Step 7: Run cutover tests**

Run:

```bash
gofmt -w cmd internal test
go test ./internal/... -count=1
go test ./test/integration -count=1
go build ./cmd/llmgw
```

Expected: PASS; the binary imports CLIProxyAPI but no native provider packages remain.

- [ ] **Step 8: Commit**

```bash
git add -A cmd internal test go.mod go.sum
git commit -m "Cut over to embedded CLIProxyAPI

[+] compose proxy governance and workers in one process
[+] drain queued usage during graceful shutdown
[+] keep gated live provider smoke tests
[-] remove native provider and protocol proxy implementations"
```

---

### Task 13: Update container deployment, operations documentation, CI, and notices

**Files:**
- Modify: `.env.example`
- Modify: `README.md`
- Modify: `Dockerfile`
- Modify: `docker-compose.yml`
- Modify: `CLAUDE.md`
- Modify: `.github/workflows/ci.yml`
- Delete: `.github/workflows/e2e-codex.yml`
- Create: `third_party/CLIProxyAPI/LICENSE`
- Create: `THIRD_PARTY_NOTICES.md`

**Interfaces:**
- Consumes: final CLI/config/runtime
- Produces: reproducible deployment and operator handoff

- [ ] **Step 1: Update deployment inputs**

`.env.example` must contain only:

```dotenv
LLMGW_CONFIG=/etc/llmgw/config.yaml
LLMGW_POSTGRES_DSN=postgres://llmgw:CHANGE_ME@host.docker.internal:5432/llmgw?sslmode=disable
LLMGW_KEY_PEPPER=CHANGE_ME_TO_AT_LEAST_32_RANDOM_BYTES
LLMGW_IMAGE_TAG=latest
```

Remove session-key seeds, native client spoof versions, default project, tags, and native provider flags.

In the runtime image, create `/var/lib/llmgw/cliproxy-auth` owned by numeric distroless user/group `65532:65532` before declaring or mounting it. This ensures Docker initializes a new named volume with writable ownership while the process remains non-root.

In Compose, mount the operator config read-only at `/etc/llmgw/config.yaml`, mount a named persistent writable volume at `/var/lib/llmgw/cliproxy-auth`, publish only `127.0.0.1:8088:8088`, and keep PostgreSQL external. The YAML must use `host: 0.0.0.0`, `port: 8088`, and `auth-dir: /var/lib/llmgw/cliproxy-auth`; the host-side config should be mode `0600` because native API-key provider entries may contain credentials.

- [ ] **Step 2: Update README around the new product boundary**

Document:

```text
one binary embeds CLIProxyAPI; there is no second service
copy config.example.yaml and .env.example
generate a 32+ byte pepper with a CSPRNG
run migrations/startup
llmgw auth login/list/import-legacy
llmgw key create/list/rotate/revoke
llmgw budget set/list/delete
llmgw usage show/resolve
Authorization Bearer and x-api-key examples
one key per deployed client, many keys per project
Claude Code/OpenCode/Hermes use the same base URL plus their normal API-key setting
A tunnel or reverse proxy may sit in front, but project keys remain mandatory
direct exposure requires TLS termination
calls are exact; token/cost budgets stop after crossing
notional subscription cost is not an invoice
abrupt crashes can lose an undetectable tail of multi-attempt usage
provider cooldown/retry defaults and how to tune them
rollback and legacy table/auth-import behavior
```

Use `/healthz`, never the removed `/health`.

- [ ] **Step 3: Ship upstream license material**

Copy the exact MIT license text from the pinned CLIProxyAPI commit into `third_party/CLIProxyAPI/LICENSE`. `THIRD_PARTY_NOTICES.md` must name:

```text
CLIProxyAPI
https://github.com/router-for-me/CLIProxyAPI
v7.2.102
8423cce2d1004e80948a9e2c60ee69354c0aabc3
MIT
```

- [ ] **Step 4: Update CI**

Keep formatting, vet, build, and race checks. Add the real-SDK hermetic suite as a separate step:

```yaml
- name: Domain and adapter tests
  run: go test -race ./internal/... -count=1

- name: Embedded CLIProxy integration
  run: go test ./test/integration -count=1 -v
```

Do not add live credentials or enable `test/e2e` in automatic CI.

Delete `.github/workflows/e2e-codex.yml`: its direct refresh-token rotation and old `LLMGW_CODEX_*` test inputs belong to the removed native adapter. Gated live verification now runs only with an operator-owned `LLMGW_LIVE_CONFIG`; do not recreate provider-token handling in GitHub Actions.

- [ ] **Step 5: Verify deployment artifacts**

Run:

```bash
go test ./internal/... ./test/integration -count=1
go vet ./...
go build ./cmd/llmgw
docker build -t llmgw:cliproxy-pivot .
docker run --rm llmgw:cliproxy-pivot --help
```

Expected: all Go checks PASS; image builds; help lists `serve`, `auth`, `key`, `budget`, and `usage`.

- [ ] **Step 6: Commit**

```bash
git add .env.example README.md Dockerfile docker-compose.yml CLAUDE.md .github/workflows third_party/CLIProxyAPI/LICENSE THIRD_PARTY_NOTICES.md
git commit -m "Document governed proxy deployment

[+] add single-file configuration and project-key operations
[+] ship CLIProxyAPI license notice
[&] run embedded SDK integration tests in CI"
```

---

### Task 14: Run the release gate and inspect the final diff

**Files:**
- Modify only files whose owning test identifies a defect

**Interfaces:**
- Consumes: complete implementation
- Produces: evidence that the pivot is ready for review

- [ ] **Step 1: Format and reject forbidden module state**

Run:

```bash
gofmt -w cmd internal test
test -z "$(gofmt -l cmd internal test)"
test -z "$(rg '^replace ' go.mod || true)"
go mod tidy
git diff --check
```

Expected: every command exits 0 and `go mod tidy` leaves no unexplained dependency drift.

- [ ] **Step 2: Run the complete automated gate**

Run:

```bash
go test -race ./internal/... -count=1
go test ./test/integration -count=1 -v
go test ./test/e2e -count=1
go vet ./...
go build ./...
```

Expected: internal/integration tests PASS; gated live tests SKIP without `LLMGW_LIVE_CONFIG`; vet/build PASS.

- [ ] **Step 3: Audit security-sensitive strings and routes**

Run:

```bash
rg 'X-Project|X-Tags|LLMGW_DEFAULT_PROJECT|LLMGW_SESSION_KEYS|LLMGW_CODEX_ACCOUNTS' --glob '!docs/specs/**' --glob '!docs/plans/**' --glob '!docs/superpowers/**'
rg 'api-keys:|secret-key:|management.html|/v0/management|/anthropic/callback|/codex/callback' internal test config.example.yaml
rg 'Source|Failure.Body|ResponseHeaders|RequestLogging' internal/adapter/cliproxy
```

Expected: the first command finds only archived migration comments/history when explicitly retained; the second and third find only validation, hard-deny, field-dropping, and tests.

- [ ] **Step 4: Inspect runtime dependency direction**

Run:

```bash
go list -deps ./cmd/llmgw | rg 'internal/adapter/(httpserver|provider)|internal/domain/llm'
go list -m github.com/router-for-me/CLIProxyAPI/v7
git status --short
git diff --stat
```

Expected: no old runtime package; SDK is `v7.2.102`; only intentional implementation files are modified.

- [ ] **Step 5: Run gated live smoke only when credentials are intentionally available**

Run:

```bash
LLMGW_LIVE_CONFIG=/absolute/path/to/live-test-config.yaml go test ./test/e2e -count=1 -v
```

Expected: one streaming and one non-streaming request per configured production protocol PASS, usage rows appear, and logs contain no secrets. If no isolated live credentials are available, leave this gate documented as not run rather than substituting production credentials into CI.

- [ ] **Step 6: Prepare the review handoff**

Include in the handoff:

```text
exact SDK tag and commit
automated commands and results
whether gated live tests ran
migration/rollback warning
known abrupt-crash tail-accounting limitation
first deployment steps: auth import/login, key creation, budget creation, cutover
```

Do not merge until the schema migration, exclusive access behavior, streaming accounting, and container startup have each been reviewed.
