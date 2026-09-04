# LLMGW

LLMGW is a governed LLM gateway. One Go binary embeds
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI); it is one process,
one container, and one listener. There is no second service, child process, or
reverse proxy between LLMGW and the embedded SDK.

LLMGW authenticates every client with a project key, applies project budgets,
and records normalized request and usage attempts in PostgreSQL. CLIProxyAPI
continues to own provider protocols, provider authentication, model routing,
cooldown, and retry behaviour.

## Deploy

LLMGW uses one shared YAML file. Native CLIProxyAPI fields are at the YAML
root; LLMGW-owned settings are under `llmgw`. Keep provider credentials in the
YAML only when the native SDK requires them, otherwise use your secret manager
to render the file. PostgreSQL credentials and the project-key pepper remain
environment/secret-manager values.

```sh
cp config.example.yaml config.yaml
cp .env.example .env
chmod 600 config.yaml
```

`config.yaml` is bind-mounted read-only and may contain native API-key provider
entries, so its host-side mode must remain `0600`. The example binds the
container listener to `0.0.0.0:8088`; Compose publishes that listener only on
`127.0.0.1:8088`. It mounts the persistent `cliproxy-auth` named volume at
`/var/lib/llmgw/cliproxy-auth`. PostgreSQL is deliberately external: create an
`llmgw` database and give the configured role access before startup.

Run exactly one active `llmgw serve` for a PostgreSQL database and its
persistent CLIProxyAPI auth directory. A dedicated PostgreSQL session lock
rejects a second server that shares the database before startup recovery can
mutate live rows. Do not scale replicas, share one auth directory across
independent databases, or use an overlapping/rolling rollout. Stop the old
server completely before starting its replacement.

Generate and store a stable pepper with a cryptographically secure random
source; it must be at least 32 bytes. For example:

```sh
openssl rand -base64 48
```

Put that value in `LLMGW_KEY_PEPPER` and set `LLMGW_POSTGRES_DSN` in `.env`.
The `.env.example` file has these deployment inputs: `LLMGW_CONFIG`,
`LLMGW_POSTGRES_DSN`, `LLMGW_KEY_PEPPER`, `LLMGW_IMAGE_TAG`, and the optional
`LLMGW_DISCORD_WEBHOOK_URL`. Leaving it empty or absent disables Discord
alerting entirely; enabling it requires outbound HTTPS from the container to
`discord.com`. When a webhook is configured and Discord is unreachable, `key
create`, `key rotate`, `auth login`, and `auth import-legacy` take up to five
seconds longer to exit — the command still succeeds, and the delivery failure
is only a warning on stderr.

The `llmgw` block of the shared YAML holds the rest. `postgres-dsn-env`,
`key-pepper-env`, and `discord-webhook-url-env` name the environment variables
that carry those secrets rather than the secrets themselves, and default to the
`.env.example` names above. `usage-retention-days` keeps usage history for that
many whole days (default 35, minimum 2, which is what the daily budget window
needs). `usage-outstanding-capacity` bounds the admitted generations still
waiting for their usage record (default 64, maximum 1024).

Start the single service; migrations run during startup:

```sh
docker compose up -d
docker compose logs -f llmgw
curl -fsS http://127.0.0.1:8088/healthz
```

For a release rollout, set `LLMGW_IMAGE_TAG` to the selected immutable release
tag and run `docker compose pull`. Route traffic away, stop the existing
service with `docker compose down`, verify no other `llmgw serve` instance uses
the database or auth directory, then run `docker compose up -d`. The same
shared configuration works for a host binary with
`LLMGW_CONFIG=./config.yaml llmgw serve`; host-binary upgrades follow the same
stopped-traffic cutover.

## Provider authentication and projects

Provider credentials belong to CLIProxyAPI's persistent auth directory, not to
the LLMGW database. Use the local administrative commands against the shared
configuration:

```sh
llmgw auth login claude
llmgw auth list
llmgw auth import-legacy

llmgw key create analytics --name claude-code
llmgw key list analytics
llmgw key rotate KEY_ID --overlap 15m
llmgw key revoke KEY_ID
```

`key create` prints the plaintext project key once. Store it in the deployed
client's secret store. Create one key per deployed client; a project can own
many keys. A key is mandatory for every generation and metadata request,
whatever network layer sits in front of it.

Use either standard header form (but not conflicting values):

```sh
curl http://127.0.0.1:8088/v1/chat/completions \
  -H 'Authorization: Bearer LLMGW_PROJECT_KEY'

curl http://127.0.0.1:8088/v1/chat/completions \
  -H 'x-api-key: LLMGW_PROJECT_KEY'
```

Claude Code, OpenCode, and Hermes Agent all use the same LLMGW base URL and
their normal API-key setting; set that API-key value to the project key issued
for that deployed client. The embedded SDK serves its supported provider
protocol routes, including Anthropic Messages, OpenAI Chat
Completions/Responses, and Gemini where configured.

## Projects

A project is created by issuing its first key; there is no separate create
command. Beyond its keys, a project carries two settings the gateway applies to
every request it authenticates:

```sh
llmgw project list
llmgw project effort analytics xhigh
llmgw project markup-guard analytics on
```

`project list` prints each project's name, creation time, default effort, and
markup-guard state.

`project effort NAME low|medium|high|xhigh|max|none` sets a default Anthropic
thinking effort, injected as `output_config.effort` into every generation
request that names none of its own. A request that already carries an effort is
left alone, whatever its value, as is one whose client disabled thinking —
Anthropic refuses a level above `high` beside disabled thinking. `none` removes
the default.

`project markup-guard NAME on|off` makes the gateway refuse a non-streamed
generation response whose tool-call inputs carry leaked function-call markup,
turning it into a retryable upstream error rather than a corrupted success the
client consumes. Streamed responses pass through untouched: their tool inputs
arrive as partial JSON deltas no complete-document scan can screen.

## Budgets and usage

Manage a project's limits, then read back what it consumed:

```sh
llmgw budget set analytics --dimension calls --window day --max 1000 --action block
llmgw budget list analytics
llmgw budget delete LIMIT_ID

llmgw usage show analytics --since 24h --by model
llmgw usage resolve REQUEST_ID --assume-zero
```

Budgets cover `calls`, `tokens`, or notional `cost` across an hourly or daily
window. Calls are counted exactly. Token and cost budgets are admitted using
what has already been recorded, then stop subsequent calls after a completed
request crosses the limit; they cannot revoke an upstream request already in
flight. Notional subscription cost is an accounting estimate, not a provider
invoice.

An abrupt crash can leave an undetectable tail of multi-attempt usage after a
request has been admitted. Such requests are marked for accounting resolution;
use `usage resolve REQUEST_ID --assume-zero` only after operator review and
only when zero is the intended conservative resolution.

The example's native defaults retry once (`request-retry: 1`) and try at most
two provider credentials (`max-retry-credentials: 2`). CLIProxyAPI places
provider/auth failures in cooldown according to its native settings. Tune
`request-retry`, `max-retry-credentials`, `max-retry-interval`,
`transient-error-cooldown-seconds`, and the credential pool in `config.yaml`
to match the provider and desired latency. Preserve the required security
settings in the example: remote management, the control panel, home, and pprof
must remain disabled.

## What the gateway rewrites on Anthropic requests

Two routes are rewritten on their way upstream: `POST /v1/messages` and `POST
/v1/messages/count_tokens`, both in the Anthropic Messages format. Every other
protocol route the embedded SDK serves passes through untouched.

- **Tool names.** Every client-defined tool — a declaration with no `type`,
  or with `type: "custom"` — is sent upstream as `mcp__llmgw__<name>`,
  consistently in `tools[]`, in `tool_choice`, and in the `tool_use` blocks
  replayed from the conversation history; the original name is restored in
  responses, streaming included, so clients never see the prefix. Tools
  Anthropic itself defines, recognised by their `type` and never by their name,
  names already starting with `mcp__`, and names longer than 52 characters are
  left as they are. The reason is the prompt cache: on OAuth credentials the
  embedded SDK rewrites client tool names with a per-request value, which
  changes the very start of the prompt on every call and defeats the provider's
  cache. A name already in the MCP shape is forwarded verbatim, so the cached
  prefix stays stable. `count_tokens` receives the same rewrite, so the count
  matches the payload actually sent. Source: `internal/domain/toolprefix`.
- **Context editing.** A generation payload that carries no
  `context_management` gets `{"edits": []}`. Left absent, the field is filled
  in by the SDK with a thinking-clearing strategy of its own, which rewrites
  the conversation prefix on every turn and makes the cache unusable. A client
  that sends its own `context_management` keeps it. Source:
  `internal/domain/contextedit/claim.go`.
- **Default effort.** The project's default thinking effort, injected as
  described above. Source: `internal/domain/effort`.

Session affinity is the same concern one layer down. `config.example.yaml`
enables `routing.session-affinity` so a conversation sticks to one upstream
credential: the provider's prompt cache lives per account, and a multi-turn
agent loop that round-robins across a pool re-pays its whole context. Keep it
enabled.

## CI and live verification

Automatic CI runs formatting, vet, build, race-checked domain/adapter tests,
and the hermetic embedded CLIProxy integration suite. It has no live provider
credentials and does not run `test/e2e`. Operator-owned live verification is
explicitly gated by `LLMGW_LIVE_CONFIG` and is never recreated as a GitHub
Actions provider-token workflow.

## License notice

LLMGW embeds CLIProxyAPI as a library, from pristine upstream sources at the
version pinned in `go.mod`. Its MIT license is included in
[`third_party/CLIProxyAPI/LICENSE`](third_party/CLIProxyAPI/LICENSE); see
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) for the pinned-source notice.

Upgrading the SDK is a normal module upgrade — there is nothing to reapply.
Keep `disable-image-generation` set to `true` and leave payload write rules
empty: together they guarantee that each upstream attempt reports exactly one
usage record, which is what the gateway's accounting relies on. Both are
enforced at startup.
