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

## Operations

### Recovering a dead refresh token

The gateway refreshes Claude Max OAuth tokens automatically. It does **not** perform
interactive OAuth re-authentication. When a refresh is rejected with `invalid_grant`
(the stored `refresh_token` is revoked or expired), the refresh fails with a
`DeadRefreshTokenError` and that account stops serving traffic until an operator
re-seeds it.

To recover:

1. Obtain a fresh Claude Code OAuth `refresh_token` for the account.
2. Update the credential the gateway reads on the next refresh:
   - **Existing DB:** update the matching `oauth_token` row's `refresh_token`
     (`UPDATE oauth_token SET refresh_token = '<new>' WHERE account_label = '<label>';`).
   - **Fresh DB:** set `LLMGW_OAUTH_REFRESH_TOKENS` (e.g. `label=<new-token>`); the
     seed runs on startup.
3. The gateway resumes on the next request — no restart is required for the DB update.
