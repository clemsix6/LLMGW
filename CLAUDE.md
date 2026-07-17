# CLAUDE.md

This file guides Claude Code when working in the **LLMGW** repository.

LLMGW is a **local LLM gateway**: one self-hosted Go service that fronts one or more
LLM providers behind a stable API, with native per-project / per-tag usage tracking
and budget limits. It binds to localhost, is never exposed publicly, and serves only
the operator's own traffic. The design lives in `docs/specs/`.

## Go Coding Standards

### KISS Principles (CRUCIAL)

- Avoid over-engineering at all costs. Keep It Simple, Stupid.
- No unnecessary or premature abstractions.
- Prefer explicit, straightforward code over "clever" code.
- Implement only what is actually needed — no features "just in case".
- The simplest code that works is usually the best code.

### Minimal Public API (CRUCIAL)

- Keep package APIs as small as possible — fewer exported symbols is better.
- Unexported (private) by default; export only what external code strictly needs.
- Question every exported symbol: "Is this really needed by another package?"

### Documentation Requirements

- Every function, type, struct, interface, and struct field has a docstring.
- Document struct fields with inline comments.
- Use Go's standard doc format (start with the name being documented).
- All code and documentation in English.
- Do NOT put a doc comment above the `package` declaration.

### Code Style

- Maximize readability with generous spacing.
- Descriptive variable and function names.
- Go naming conventions: PascalCase (exported), camelCase (unexported).

### Function Size (CRUCIAL)

- Functions MUST be short and focused — aim for 15-25 lines, 30 maximum.
- One clear responsibility per function. Split anything longer into named helpers.
- The main function orchestrates; sub-functions execute specific tasks.

### File Size (CRUCIAL)

- Files MUST stay focused — aim for 200-300 lines, 400 maximum.
- Split by logical domain; file names clearly describe their content.
- Exception: a single cohesive type with tightly-coupled methods may exceed 400 lines
  when splitting would scatter related logic and hurt readability.

### Error Handling

- Always wrap errors with context: `fmt.Errorf("operation description:\n%w", err)`.
- Use `%w` to preserve the original error; put `\n` before `%w` for readable deep stacks.
- Never return raw errors without context.

### TODO Comments

- Mark incomplete implementations or future work with `// TODO: description`.

### go.mod Hygiene (CRUCIAL)

- It is FORBIDDEN to commit a `replace` directive. They are for local development only
  and MUST be removed before any commit that touches `go.mod`.

## Architecture

Hexagonal (ports & adapters). This file describes the **rules**, not the file tree —
the code is its own structure documentation.

- **Domain** (`internal/domain/...`): pure business logic, zero infrastructure imports.
  Budget accounting & limit evaluation, usage metering, routing decisions, the project
  model. Defines all external dependencies as **port interfaces**.
- **Adapters** (`internal/adapter/...`): infrastructure implementations of the ports —
  Postgres (state + counters + audit), LLM providers (Claude Max OAuth first, OpenRouter
  later), the HTTP server.
- **Composition root** (`cmd/...`): wiring only, no business logic.

Keep the domain ignorant of HTTP, SQL, and any provider's wire format.

## Testing (E2E-first against the real provider, CRUCIAL)

> **CI note (2026-07-17):** the real-Anthropic E2E no longer runs in CI. Anthropic now rejects the
> session-key OAuth bootstrap (authorize step) with `403 session_stale_relogin`, so a fresh CI run
> can no longer mint tokens and only reddened `main` for an infra reason. The suite below still
> lives in `test/e2e/` and is the standard for local validation — run it against a freshly
> re-authenticated credential. CI keeps only the hermetic `check` job (build/vet/gofmt/unit tests).

- **Every feature MUST be covered by end-to-end tests** that drive the real gateway over its
  HTTP API. The E2E suite **hits the real Anthropic API** — a mock would not exercise the
  OAuth + Claude Code spoof, which is the core risk. Accept non-determinism.
- Assert on response **shape and plausibility** (status, valid structure, non-empty content of a
  plausible length, expected `stop_reason`, `tool_use` when tools are used) — never exact text.
- **Retry transient API errors** (5xx / network / timeouts) with bounded backoff; never retry the
  gateway's own `402` / `503` (those are assertions).
- Harness: real gateway + real ephemeral Postgres (migrations applied) + the real Claude Max
  backend (seeded test credentials; calls consume quota and rotate the token). A **local stub
  upstream is used ONLY for failure injection** the real API won't produce on demand (forced
  429 → cooldown, all-cooling → 503, refresh failure).
- Verify budget tracking from REAL responses (tokens/cost recorded) and limits (a `calls` cap is
  deterministic; `tokens`/`cost` by crossing).
- The real-API suite is gated by test-credential presence. Domain unit tests cover pure-logic
  edge cases (budget arithmetic, window boundaries) without network. E2E is the backbone.

## Git Workflow

- `main` is the only long-lived branch. Never push to `main` directly.
- One short-lived branch per task, named `type/kebab-description` where `type` is one of
  `feat | fix | refactor | chore | docs | perf`.
- Squash-merge the PR, then delete the branch.

## Git Commit Convention

Title line: NO prefix — a few words describing the purpose of the commit.
Then a blank line, then the detailed changes, one per line, each with a prefix:

- **[+]** addition · **[-]** removal · **[&]** change/refactor/update · **[!]** bug fix

One change per line, minimal words. List as many as needed.

**IMPORTANT**: NO footers, NO "Generated with...", NO "Co-Authored-By", NO emojis.

Example:

    Postgres budget counters

    [+] usage_event table + windowed SUM counters (hourly/daily)
    [+] budget_limit evaluation across calls/tokens/cost dimensions
    [&] move pricing into model_price table
    [!] persist the rotated OAuth refresh_token
