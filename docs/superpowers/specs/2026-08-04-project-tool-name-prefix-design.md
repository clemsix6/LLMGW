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
chooses its mode when the status and headers are written:

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

The rewrite itself is two pure functions over `[]byte` in the domain — one for
the outbound payload, one for an inbound payload or event — with no HTTP, SQL,
or SDK import, and no knowledge of where the bytes came from. They are the unit
under test for every rewrite rule.

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
skipped rather than treated as an error. The only new rejection is the 32 MiB
request ceiling.

If the project flag cannot be read, that is an authentication failure, and it
already produces `503` through the existing path — no new case.

## 11. Testing

The integration suite drives the real gateway over HTTP against a stub upstream,
which is what makes the assertions here possible: the stub records exactly what
it received.

- A project with the flag off sends a request naming `search_web`; the stub sees
  `search_web`.
- A project with the flag on sends the same request; the stub sees
  `new_search_web` in the declarations, in `tool_choice`, and in the replayed
  history.
- The stub returns a `tool_use` block naming `new_search_web`; the client sees
  `search_web`, both non-streamed and streamed.
- The stub returns a streamed `tool_use` split across several writes; the client
  still sees one coherent `content_block_start` naming `search_web`.
- The stub returns a `tool_use` naming something without the prefix; the client
  sees it unchanged.
- A `count_tokens` request from a flagged project reaches the stub prefixed.
- A non-200 upstream response reaches the client unchanged.

Domain tests cover the rewrite functions directly: each of the three outbound
locations, the inverse on both response shapes, malformed JSON, absent fields,
and a name that does not carry the prefix.

Assertions are on shape and plausibility, never on model text.

## 12. Known Limitations

- **Anthropic server tools break.** A server tool (`web_search_20250305`,
  `code_execution`, `computer`, `bash`, `text_editor`) declared by a flagged
  project is prefixed like any other, and Anthropic will not recognise the
  renamed tool. This is the accepted cost of renaming every tool without an
  exemption list. A project using server tools must leave the flag off.
- **Errors leak the prefix.** An Anthropic error message that quotes a tool name
  reaches the client with `new_` still attached, because error bodies are
  forwarded unchanged.
- **Long names fail upstream.** A tool name within four characters of the
  128-character API limit exceeds it once prefixed, and Anthropic rejects the
  request. LLMGW does not pre-empt this.
- **Text is never rewritten.** A tool name written in prose — in a system
  prompt, in message text, or inside tool arguments — is left alone in both
  directions.

## 13. Deployment Notes

- Migration `0014` runs at startup like every other; it adds a column with a
  default and needs no backfill.
- No configuration change. The feature is inert until an operator runs
  `llmgw project tool-prefix <name> on`.
- No new environment variable, secret, or service ordering constraint.
