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
including traffic arriving through Cloudflare Tunnel.

Use either standard header form (but not conflicting values):

```sh
curl http://127.0.0.1:8088/v1/chat/completions \
  -H 'Authorization: Bearer LLMGW_PROJECT_KEY'

curl http://127.0.0.1:8088/v1/chat/completions \
  -H 'x-api-key: LLMGW_PROJECT_KEY'
```

Claude Code, OpenCode, and Hermes all use the same LLMGW base URL and their
normal API-key setting; set that API-key value to the project key issued for
that deployed client. The embedded SDK serves its supported provider protocol
routes, including Anthropic Messages, OpenAI Chat Completions/Responses, and
Gemini where configured.

## Budgets and usage

Create a project by issuing its first key, then manage its project-wide limits:

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

## Network safety and rollback

Cloudflare Tunnel is the recommended way to publish LLMGW, but it is a network
control rather than client authentication: project keys remain mandatory. If
you expose LLMGW directly, put TLS termination in front of it and restrict the
listener/firewall as appropriate. Do not publish the Compose port on a routable
host interface without that protection.

Before the first migration-0010 upgrade, make and test a restorable PostgreSQL
backup while the old service is stopped. Also snapshot the persistent
CLIProxyAPI auth directory and retain the prior image tag, `config.yaml`, and
`.env`. Migration 0010 renames the historical tables to
`legacy_usage_event`, `legacy_budget_limit`, and `legacy_model_price`, and
drops `reservation`; therefore an image-only rollback after 0010 is
impossible.

To return to the previous image, first route traffic away and stop every LLMGW
instance. Restore the tested pre-0010 PostgreSQL backup before starting the
previous image. Restore the matching auth-directory snapshot as well, or
explicitly verify and account for every auth-file change made since the
snapshot. Only after both state stores are compatible should you select the
prior image tag and start the old service. `llmgw auth import-legacy` reads
historical `oauth_token` rows without modifying them and writes compatible
provider-auth files into the persistent auth directory; review its
per-credential status and use `auth login` for entries marked `needs-login`.

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
