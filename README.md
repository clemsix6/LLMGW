# LLMGW

A **local LLM gateway**: one self-hosted Go service that fronts LLM providers behind a
stable API, with native per-project / per-tag usage tracking and budget limits.

- Drop-in **Anthropic Messages** API (`POST /v1/messages`) — point any Anthropic SDK at it.
- Governance via headers: `X-Project`, `X-Tags`. Projects auto-create on first use.
- Per-`(project, tag)` budgets in **calls / tokens / cost**, hourly + daily, hard-block.
- **V1 backend:** Claude Max via OAuth (a maintained Go reimplementation of clewdr's
  `/code` path, without the flagged TLS fingerprint). **Later:** Anthropic API keys,
  OpenRouter (any LLM).
- Local only (binds `127.0.0.1`), no auth, Postgres-backed state.

Design: [`docs/specs/2026-06-18-llmgw-design.md`](docs/specs/2026-06-18-llmgw-design.md).
