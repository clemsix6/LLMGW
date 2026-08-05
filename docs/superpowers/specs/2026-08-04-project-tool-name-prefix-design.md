# Project Tool-Name Prefix Design

**Date:** 2026-08-04
**Status:** Draft

## 1. Decision

A project can opt into having every tool it declares renamed on its way upstream
and renamed back on its way down. With the option enabled, a client that
declares `search_web` sends a request in which Anthropic sees `new_search_web`,
and a `tool_use` block naming `new_search_web` reaches the client naming
`search_web` again. The client never sees the prefix; nothing about its
integration changes.

The prefix is the constant `new_`. The option is a per-project boolean, off by
default, and it applies only to the Anthropic message format.

## 2. Why

The embedded SDK cloaks requests as Claude Code by default: `cloak_mode` falls
back to `auto`, and `auto` means cloak whenever the client's `User-Agent` does
not start with `claude-cli`. Cloaking replaces the client's `system` array with
Claude Code's own blocks and moves the client's instructions into the first user
message.

A project's tool names therefore arrive at Anthropic inside a request that
presents itself as Claude Code. Names that match what the model associates with
Claude Code's native tools compete with that framing. Prefixing every tool name
puts the project's tools in a namespace of their own, and doing it inside the
gateway keeps every client unchanged.

## 3. Goals

- A project opts in; every other project is byte-for-byte unaffected, with no
  buffering, copying, or response wrapping on its path.
- The rewrite is invisible to the client in both directions, streaming included.
- Tool names stay consistent within a request: declarations, forced choice, and
  conversation history are renamed together.
- The rewrite lives where LLMGW already owns the request, and consumes the SDK
  unmodified.

## 4. Non-Goals

- No coverage of the OpenAI or Gemini client formats. Only the Anthropic message
  format carries the rewrite.
- No configurable prefix. `new_` is a constant; a project turns it on or off.
- No exemption list. Every tool a project declares is renamed (see §12).
  **Superseded:** tools Anthropic defines are now left alone, discriminated by
  the presence of a `type` field rather than by any list of names. See §13.
- No rewriting of tool names that appear in free-form text — system prompts,
  message text, or tool arguments are never inspected.

## 5. Trigger Conditions

The rewrite runs when all three hold:

1. The authenticated project has the option enabled.
2. The request path is `POST /v1/messages` or `POST /v1/messages/count_tokens`.
3. The request passed authentication and admission.

`count_tokens` is included because it carries the same payload shape. Excluding
it would count tokens for a payload that differs from the one actually sent, and
the client would see a number it cannot reconcile with its bill. It has no
response rewrite: its response carries no tool names.

Requests from a project without the option take exactly today's path. The body
is not read, no writer is wrapped, and no allocation is made on their behalf.

The rewrite runs after admission, not before it. A request the budget rules
block never reaches the upstream, so buffering and rewriting its body would be
work done for a response that is already decided.

## 6. Data Model and Operator Surface

`project` gains one column:

```sql
ALTER TABLE project ADD COLUMN prefix_tool_names boolean NOT NULL DEFAULT false;
```

as migration `0014_project_tool_prefix.sql`.

`KeyByPublicID` already joins `project` to read its name, so the column is added
to that `SELECT` and carried on `governance.ClientKey`, then onto
`governance.KeyIdentity` as `PrefixToolNames`. The middleware reads it from the
identity it already holds in `admit`; it is not added to `RequestIdentity` and
does not enter the request context, because the only consumer is the middleware
itself.

Projects are created implicitly today by `key create`, and there is no `project`
command. One is added, with the minimum needed to operate the flag:

```
llmgw project list
llmgw project tool-prefix <name> on|off
```

`list` prints each project's name, creation time, and prefix state. `tool-prefix`
sets the flag and fails on an unknown project rather than creating one — implicit
creation stays a property of `key create` alone.

## 7. Outbound Request Rewrite

The Anthropic message format carries a tool name in three places, all of which
are renamed:

- `tools[].name` — the declarations.
- `tool_choice.name` — present only when `tool_choice.type` is `tool`.
- `messages[].content[].name` — on blocks whose `type` is `tool_use`, which is
  the conversation history the client replays.

The history is not optional. A request that declares `new_search_web` while
replaying an assistant turn that called `search_web` is internally inconsistent
and Anthropic rejects it. `tool_result` blocks reference their call through
`tool_use_id` and carry no name, so they are left alone.

A name is prefixed unconditionally, without inspecting what it is. Renaming is
pure string concatenation: `new_` + the original name.

The middleware reads the body into memory, applies the rewrite, and replaces
`c.Request.Body` with a reader over the result, updating `ContentLength` and
clearing any conflicting `Content-Length` header. Bodies above 32 MiB are
rejected with `413` and the error type `request_too_large` in the existing
envelope, rather than being buffered — 32 MiB is above Anthropic's own practical
request ceiling, so the limit is reached only by a request that would fail
upstream anyway.

A body that is not valid JSON is passed through untouched. Rejecting it here
would replace Anthropic's specific validation error with a vaguer one from us.

## 8. Inbound Response Rewrite

The middleware wraps `c.Writer` before calling the SDK handler. The wrapper
chooses its mode at the first write or flush — not when the status is set, which
the SDK's handlers never do through the wrapper — and locks it once chosen:

- **Streaming** (`Content-Type: text/event-stream`) — headers pass through
  immediately, because a client waiting on a stream must not be held back. The
  wrapper accumulates written bytes until it sees the `\n\n` event delimiter,
  parses that event, and rewrites it before passing it on. Only events of type
  `content_block_start` whose `content_block.type` is `tool_use` are touched,
  and only their `content_block.name`. Everything else is forwarded byte for
  byte. `Flush` forwards to the underlying writer after any complete events have
  been passed on, so streaming latency is unchanged.

- **Buffered** (anything else) — the wrapper holds the status line, headers, and
  body until the handler returns, rewrites `content[].name` on blocks whose
  `type` is `tool_use`, then writes the result with a corrected
  `Content-Length`. Holding the headers is what makes the corrected length
  possible: the rewrite shortens the body, so the length written before the
  rewrite would be wrong.

In both modes, only `200` responses are rewritten. Error bodies carry no
`tool_use` blocks, and passing them through unchanged keeps upstream error
reporting intact.

Removal is the exact inverse of §7: a name is stripped only if it actually
starts with `new_`. A name without the prefix — a model naming a tool that was
never declared, for instance — is forwarded unchanged rather than being
truncated or rejected.

Two bounds keep the wrapper from growing without limit. In streaming mode, if
the accumulated buffer passes 1 MiB without a delimiter, it is forwarded as-is
and accumulation restarts: a stream that never delimits is a stream we should
not be holding. In buffered mode the same 32 MiB ceiling as the request applies;
past it the wrapper stops rewriting and forwards what it has.

## 9. Code Layout

The rewrite itself is three pure functions over `[]byte` in the domain — one for
the outbound payload, one for a complete inbound response, one for a single
stream event — with no HTTP, SQL, or SDK import, and no knowledge of where the
bytes came from. They are the unit under test for every rewrite rule.

The adapter holds the HTTP mechanics: reading and replacing the request body,
the `gin.ResponseWriter` wrapper and its two modes, and the decision to engage
based on route and project flag. The wrapper is its own file; the middleware
gains the branch that installs it.

Persistence follows the existing shape: the column in the `KeyByPublicID` query
and the scan, the flag on the domain types, and a store method for the CLI to
set it.

## 10. Error Handling

The rewrite never fails a request that would otherwise succeed. Malformed JSON
in either direction is forwarded unchanged. A missing or unexpected field is
skipped rather than treated as an error.

Two rejections are new, both only on the flagged path: the 32 MiB request
ceiling, and a `400 invalid_request_error` when the body cannot be read at all —
a client that walked away mid-upload. The second is not reported as a gateway
fault: a client abandoning its own request says nothing about our health.

If the project flag cannot be read, that is an authentication failure, and it
already produces `503` through the existing path — no new case.

## 11. Testing

The integration suite drives the real gateway over HTTP against a stub upstream.
Two properties of that harness shape what can be asserted, and both cost work
this feature must pay for:

- The stub does not record request bodies today; it captures the authorization
  header and the status. Body capture is added as part of this feature.
- The harness configures OpenAI-compatible and Codex providers, not an
  Anthropic-format one. A `POST /v1/messages` is therefore translated before it
  reaches the stub, and arrives as `tools[].function.name`,
  `tool_choice.function.name`, and `messages[].tool_calls[].function.name`.

Assertions are stated in the format the stub actually receives. That the names
survive translation is a property worth asserting rather than a compromise: it
is what proves the rewrite is independent of the upstream.

- A project with the flag off sends a request naming `search_web`; the stub sees
  `search_web`.
- A project with the flag on sends the same request; the stub sees
  `new_search_web` in the declarations, in the forced choice, and in the
  replayed history.
- The stub returns a tool call naming `new_search_web`; the client sees
  `search_web`, both non-streamed and streamed.
- The stub streams a tool call split across several writes; the client still
  sees one coherent `content_block_start` naming `search_web`.
- The stub returns a tool call naming something without the prefix; the client
  sees it unchanged.
- A `count_tokens` request from a flagged project reports a larger
  `input_tokens` than the same request from an unflagged one. Token counting is
  answered locally by the executor and issues no upstream call, so the count
  itself is the only observable proof that the rewrite reached the payload.
- A non-200 upstream response reaches the client unchanged.
- A request rejected at 32 MiB returns the bridge to its prior capacity: a
  generation permit is reserved before the rewrite runs, and a rejection that
  leaked it would degrade capacity permanently.

Domain tests cover the rewrite functions directly: each of the three outbound
locations, the inverse on both response shapes, malformed JSON, absent fields,
and a name that does not carry the prefix.

Assertions are on shape and plausibility, never on model text.

## 12. Known Limitations

- ~~**Anthropic server tools break.**~~ **Fixed — see §13.** A tool Anthropic
  defines is no longer renamed, so a flagged project can declare one and have it
  work. What this limitation described was real: a flagged project declaring
  `{"type": "web_search_20260209", "name": "web_search"}` was refused with
  `tools.0.web_search_20260209.name: Input should be 'web_search'`.
- **Errors leak the prefix.** An Anthropic error message that quotes a tool name
  reaches the client with `new_` still attached, because error bodies are
  forwarded unchanged.
- **Long names fail upstream.** A tool name within four characters of the
  128-character API limit exceeds it once prefixed, and Anthropic rejects the
  request. LLMGW does not pre-empt this.
- **Text is never rewritten.** A tool name written in prose — in a system
  prompt, in message text, or inside tool arguments — is left alone in both
  directions.
- **The SDK's own OAuth tool remap stops matching, and it exists for a reason
  worth knowing.** On Claude OAuth credentials the SDK normalises fourteen
  lowercase tool names to Claude Code's casing on the way upstream and restores
  them on the way back: `bash`, `read`, `write`, `edit`, `glob`, `grep`, `task`,
  `webfetch`, `todowrite`, `question`, `skill`, `ls`, `todoread`,
  `notebookedit`. The SDK states its purpose plainly — Anthropic fingerprints
  tool names to detect third-party clients on OAuth traffic, and renaming to
  official names avoids extra-usage billing.

  A prefixed `new_bash` matches neither the map nor an official name. On an
  OAuth credential this feature therefore works against that mechanism rather
  than alongside it: it makes the request more distinguishable from Claude Code,
  not less. On an API-key credential the mechanism does not apply and the
  concern does not arise. A project on OAuth should weigh this before enabling
  the flag.
- **A configured non-streaming keep-alive is held back.** When
  `non-stream-keep-alive-interval` is set, the SDK emits newline frames during a
  long non-streamed generation to keep intermediaries from timing out. Buffered
  mode holds them until the response completes. The SDK default is 0, so this
  affects only a deployment that turned it on.

## 13. Anthropic-Defined Tools Are Exempt (added 2026-08-05)

A tool Anthropic defines is never renamed. The discriminator is the `type`
field, not the name: an Anthropic-defined tool declares
`{"type": "web_search_20260209", "name": "web_search"}`, while a project's own
tool declares a schema and carries no `type`, or carries `type: "custom"`. A
name is therefore prefixed when its declaration has no `type`, or when that
`type` is `custom`.

The name was rejected as the discriminator because a project legitimately
declares its own tool called `bash`, which must still be namespaced, while
Anthropic's `bash` (`type: bash_20250124`) must not. A list of exempt names
cannot tell those two apart; the `type` can, and needs no maintenance as
Anthropic adds tools.

The exemption cannot stop at the declarations. `tool_choice.name` and the
`tool_use` names replayed in history carry a name and no `type`, so the exempt
names are collected from `tools[]` first — read before any rewrite — and honoured
at all three locations. A request whose declarations name `bash` while its
history names `new_bash` is internally inconsistent and Anthropic rejects it.

Two consequences are accepted rather than solved. A name that is declared twice,
once by the project and once by Anthropic, resolves to exempt wherever only the
name is available: prefixing a name Anthropic owns fails with no recovery, while
leaving a project name alone merely forgoes the namespace. And a tool named only
in replayed history, never declared in `tools[]`, is still prefixed, because the
exemption can only be derived from the declarations.

The response path needs no change: removal strips a leading `new_` and returns
anything else untouched, and no Anthropic tool name begins with `new_`.

## 14. Deployment Notes

- Migration `0014` runs at startup like every other; it adds a column with a
  default and needs no backfill.
- Migration `0015` restores `project.created_at`, which `0012` had dropped as
  never selected and which `project list` needs again. The original values were
  not retained anywhere, so every existing project reports the migration's run
  time as its creation time. Nothing reads that column for governance — it is
  operator-facing only.
- No configuration change. The feature is inert until an operator runs
  `llmgw project tool-prefix <name> on`.
- No new environment variable, secret, or service ordering constraint.
