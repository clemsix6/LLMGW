# Project Tool-Name Prefix — Implementation Plan

**Spec:** `docs/superpowers/specs/2026-08-04-project-tool-name-prefix-design.md`
**Date:** 2026-08-04

Single phase. Everything the spec describes is implemented in this iteration.

## Cross-cutting constraints

These bind every task; a task that violates one is wrong even if its own tests
pass.

- **Rewrite JSON with `gjson`/`sjson`, never `encoding/json`.** Both are already
  in `go.sum` as indirect dependencies of the SDK; using them directly promotes
  them to direct requires, which `go mod tidy` handles. Round-tripping through
  `map[string]any` would reorder keys and mangle number precision on a payload
  that must otherwise reach Anthropic unchanged.
- **`sjson` cannot write through a `#` wildcard.** `sjson.SetBytes(payload,
  "tools.#.name", …)` does not work — `#` is rejected as a path character.
  Every wildcard rewrite is therefore: enumerate indices with `gjson`, then
  `sjson.SetBytes` on each concrete path (`tools.0.name`,
  `messages.3.content.1.name`). **Re-read from the updated bytes on each
  iteration** — a `gjson.Result` is a snapshot of the buffer it was read from,
  and reusing stale results after a `SetBytes` corrupts the document. The SDK
  documents this same trap in `internal/runtime/executor/claude_executor_request.go`.
- **No `replace` directive** ever reaches a commit.
- Every exported symbol carries a docstring; every struct field an inline
  comment. Functions stay under 30 lines, files under 400.
- The domain package imports no HTTP, SQL, or SDK package.
- Run `just` recipes for build and test; do not invent new ones.

---

## Batch 1 — The project flag, from the column to the authenticated identity

### Task 1.1 — Persist and propagate the flag

Add `internal/adapter/postgres/migrations/0014_project_tool_prefix.sql`:

```sql
ALTER TABLE project ADD COLUMN prefix_tool_names boolean NOT NULL DEFAULT false;
```

Then carry it through the read path:

- `governance.ClientKey` gains `PrefixToolNames bool`.
- `governance.KeyIdentity` gains `PrefixToolNames bool`.
- `keyIdentity` in `internal/domain/projectkey/service.go` copies the field onto
  the identity it builds.

**`scanClientKey` is shared — all four of its queries must be updated together.**
It scans for `KeyByPublicID`, `KeyByID`, `ListKeys`, and `lockedClientKey` in
`internal/adapter/postgres/keys.go`. Adding a destination to the scanner while
updating only some projections compiles cleanly and then fails at runtime with a
pgx destination-count mismatch, breaking `llmgw key list` and `llmgw key rotate`.
Every one of the four `SELECT`s gains `p.prefix_tool_names`; the two that do not
already join `project` must be given the join.

`insertClientKey` builds its `ClientKey` by hand, so `CreateKey` and `RotateKey`
would return `PrefixToolNames: false` regardless of the project's real state.
Either populate it from the project it just resolved, or leave it false
deliberately and say so in a comment — but do not leave it accidental.

`scanKeyInfo` and `governance.KeyInfo` are **not** touched: key listing has no
use for the flag.

Verify the migration runner picks the file up the same way as `0013`.

**As built.** All four `scanClientKey` queries already joined `project`, so only
the projection changed — no join had to be added. `insertClientKey` and
`ensureKeyProject` were reworked so `CreateKey` and `RotateKey` persist the
project's real flag state rather than a hardcoded false. A second migration,
`0015_restore_project_created_at.sql`, restores `project.created_at`: it had
been dropped by `0012` as never selected, and `Projects()` needs it for
`project list` (spec §6). Restoring it is append-only, following `0012`'s own
convention. Existing projects show the migration's run time as their creation
time — the original values were not retained anywhere to backfill from.

### Task 1.2 — Store methods for the operator surface

In a new focused file beside `keys.go`, add exactly two methods, no more:

- `Projects(ctx) ([]governance.Project, error)` — every project ordered by name.
  `governance.Project` gains `PrefixToolNames bool` to carry the state.
- `SetProjectToolPrefix(ctx, name string, enabled bool) error` — updates the
  named project. It must **fail on an unknown project** rather than insert one:
  implicit project creation stays a property of `key create` alone. Return a
  sentinel error the command layer can turn into a clear message.

### Task 1.3 — Integration: adapter tests

Extend the postgres adapter tests (testcontainers, following
`internal/adapter/postgres/keys_expiry_test.go`):

- A key created for a fresh project authenticates with `PrefixToolNames` false.
- After `SetProjectToolPrefix(name, true)`, `KeyByPublicID` reports it true.
- `SetProjectToolPrefix` on an unknown name returns the sentinel and creates
  nothing.
- `Projects` lists projects with their flag state.
- **`ListKeys` and the rotate path still work** — the regression Task 1.1's
  shared scanner would otherwise cause. This assertion is not optional.

---

## Batch 2 — The rewrite itself, as pure functions

New package `internal/domain/toolprefix`. No HTTP, no SQL, no SDK. The prefix is
an unexported constant `new_`. Exactly three exported functions — anything else
stays unexported.

### Task 2.1 — Outbound rewrite

`PrefixRequest(payload []byte) []byte` prefixes every tool name in an Anthropic
message payload, at three locations:

- `tools[].name` — every declaration.
- `tool_choice.name` — only when `tool_choice.type` is `tool`.
- `messages[].content[].name` — only on blocks whose `type` is `tool_use`.

Each is a wildcard rewrite: enumerate, then set concrete paths, re-reading from
the updated buffer as the cross-cutting constraints require.

Rules that matter as much as the locations:

- A payload that is not valid JSON is returned unchanged.
- An absent field is skipped, never an error.
- The name is prefixed unconditionally — no inspection of what it is, no
  exemption list, no idempotence check. A name that already starts with `new_`
  becomes `new_new_…`, which is correct: the client's namespace is its own.
- `tool_result` blocks are left alone; they reference by `tool_use_id`.
- Nothing outside these three locations is touched — not `system`, not message
  text, not tool arguments.

### Task 2.2 — Inbound rewrite

Two functions, because the two response shapes differ:

- `StripResponse(payload []byte) []byte` — a complete Anthropic response;
  strips `content[].name` on blocks whose `type` is `tool_use`.
- `StripStreamEvent(event []byte) []byte` — one SSE event; strips
  `content_block.name` only when the event's `type` is `content_block_start`
  **and** `content_block.type` is `tool_use`.

`StripStreamEvent` receives the raw event bytes as written by the SDK, including
any `event:` line and the `data: ` prefix, and must return them in the same
shape. Only the JSON after `data: ` is rewritten. **A frame with no `data:` line
is returned unchanged** — the SDK's stream keep-alive writes the comment frame
`": keep-alive\n\n"`, which must survive untouched.

Stripping is conditional: a name is changed only if it actually starts with
`new_`. A name without the prefix is returned untouched — never truncated,
never rejected.

### Task 2.3 — Integration: domain tests

`PrefixRequest`: each of the three locations independently, all three together,
a `tool_choice` whose type is not `tool` (untouched), a `tool_result` block
(untouched), malformed JSON, an empty payload, absent fields, a payload whose
message text happens to contain a tool name (untouched), and a payload with
several tools and several history blocks (proving the re-read-per-iteration
rule).

`StripResponse` and `StripStreamEvent`: the round trip against `PrefixRequest`'s
output, an event of another type (untouched), a `content_block_start` carrying
text rather than a tool (untouched), a comment frame with no `data:` line
(untouched), a name without the prefix (untouched), and malformed JSON.

---

## Batch 3 — HTTP wiring in the cliproxy adapter

### Task 3.1 — Request body replacement

A focused helper in `internal/adapter/cliproxy`. Given the gin context, it reads
the body, applies `toolprefix.PrefixRequest`, and installs the result:
`c.Request.Body` becomes a reader over the new bytes, `c.Request.ContentLength`
is set to the new length, and any `Content-Length` header is kept consistent.

Bodies above 32 MiB are rejected with `413` and the error type
`request_too_large`, through the existing `abortSafe` envelope. Use
`http.MaxBytesReader` or an equivalent bounded read — never an unbounded
`io.ReadAll`.

**The rejection must release the generation permit.** By the time the rewrite
runs, `m.bridge.reserve(request.ID)` has taken a slot, and the bridge's capacity
is finite: a leaked permit is a permanent capacity reduction, and enough of them
poison the bridge and stop the service. Follow the pattern the budget abort
already establishes a few lines above in `middleware.go` — an explicit
`m.bridge.release(request.ID)` before the abort, because the abort returns
before the barrier defer is registered. Do not invent a different mechanism; do
not assume `publishBarrier` covers it.

A read error on a body the client abandoned must not be reported as a gateway
fault: abort without touching the alert tracker's database health.

### Task 3.2 — Response writer wrapper — **COMPLEX, dispatch on opus**

New file in `internal/adapter/cliproxy`. A type implementing
`gin.ResponseWriter` that delegates to the original and rewrites tool names on
the way out.

**Mode selection is lazy, at the first write — not at `WriteHeader`.** The SDK's
Claude handlers set the content type with `c.Header(...)` and then call
`c.Writer.Write(...)`; they never call `WriteHeader` on the wrapper, and gin's
own `WriteHeader` only records the status, committing it later from inside the
delegate. A wrapper that decides at `WriteHeader` never selects a mode at all on
exactly the responses it exists to rewrite. Decide on the first `Write`,
`WriteString`, or `WriteHeaderNow`, reading `Status()` and
`Header().Get("Content-Type")` from the delegate at that moment.

**Once chosen, the mode is locked.** Mid-stream the SDK calls `c.Status(...)` to
report a terminal error after SSE headers are already on the wire; a wrapper
that re-decides would flip a live stream into non-200 passthrough mid-flight.

The three modes:

- **Non-200** — pure passthrough, whatever the content type. Error bodies carry
  no tool names and must reach the client verbatim.
- **`Content-Type: text/event-stream`** — streaming mode. Headers pass through
  immediately. Written bytes accumulate until the `\n\n` delimiter; each
  complete event goes through `toolprefix.StripStreamEvent` and is forwarded;
  the remainder stays buffered. `Flush` forwards to the underlying writer after
  any complete events have been passed on. If the buffer passes 1 MiB with no
  delimiter, forward it as-is and restart accumulation.
- **Anything else** — buffered mode. Status, headers, and body are held; at
  finalization the body goes through `toolprefix.StripResponse` and is written
  with a corrected `Content-Length`. Past 32 MiB, stop rewriting and forward
  what has accumulated. Leading whitespace may precede the JSON when a
  non-streaming keep-alive is configured — trim it before parsing rather than
  treating the body as malformed.

**Finalization covers both modes.** In buffered mode it writes the rewritten
body. In streaming mode it flushes any residual bytes unchanged — dropping them
truncates the response, and an SSE body not ending in `\n\n` fails to parse in a
way that reads as a protocol error rather than as the truncation it is.

The full `gin.ResponseWriter` surface must behave: `Write`, **`WriteString`**,
`WriteHeader`, **`WriteHeaderNow`**, `Flush`, `Hijack`, `CloseNotify`, `Pusher`,
`Size`, `Status`, `Written`. `WriteString` and `WriteHeaderNow` are the two an
embedded delegate silently forwards past the buffer, which reorders output in
buffered mode and interleaves unrewritten bytes in streaming mode.

### Task 3.3 — Integration: wire it into the middleware — **COMPLEX, dispatch on opus**

In `admit` (`internal/adapter/cliproxy/middleware.go`), after admission
succeeds: when the identity carries `PrefixToolNames` and the path is
`/v1/messages` or `/v1/messages/count_tokens`, apply the request rewrite, and
for `/v1/messages` only, install the writer wrapper.

`count_tokens` gets the request rewrite but no wrapper: its response carries no
tool names.

**Defer ordering is the real work here.** The existing defers in `admit` are
LIFO and their ordering is load bearing and commented — the barrier publishes
only after detached completion returns. The wrapper's finalization must run
before both, so the response is fully written before the request is recorded
complete. Read those comments before adding anything.

A project without the flag must take a path with no buffering, no wrapper, and
no allocation. Keep the branch cheap and obvious.

Adapter tests: the wrapper's three modes against a recording writer, including
an event split across several `Write` calls and one that arrives whole, a
`WriteString` path, and a mid-stream status change; the middleware engaging only
for the right route and flag combination; and a 32 MiB rejection returning the
bridge to its prior capacity.

**As built.** The wiring landed in three files rather than two: a
`tool_prefix.go` holds the route predicates and the middleware's branch, because
folding them into `middleware.go` would have crossed the 400-line rule.

Six behaviours the plan did not predict, each found against the real SDK:

- `Flush` also selects the mode. The SDK's Claude streaming handler has a path —
  stream closed with no data — that sets SSE headers and flushes without ever
  writing, and a wrapper ignoring `Flush` would never commit those headers.
- `WriteHeader` is suppressed once a mode is locked in **all three** modes, not
  only streaming, mirroring gin's own refusal to change a status after the first
  write.
- `Written()` and `Size()` report the wrapper's view, not the delegate's: the
  SDK reads `Written()` before appending an error body, and a held buffered
  response that read as unwritten would get an error appended on top of it.
- Oversized bodies are refused on the declared `ContentLength` before a byte is
  read, with `MaxBytesReader` catching the chunked case — spec §7 says refused
  "rather than being buffered", which the reader alone would not honour.
- A read error on an abandoned body aborts with `400 invalid_request_error`; the
  plan required only that the alert tracker not be touched, which it is not.
- Buffered leading whitespace from a configured keep-alive is trimmed for
  parsing and re-emitted in front of the rewritten JSON, so tool names remain
  the only bytes that change.

---

## Batch 4 — Operator surface and end-to-end proof

### Task 4.1 — The `project` command

New `internal/command/project.go`, following `key.go`'s shape exactly
(`flag.NewFlagSet`, `Streams`, usage helper):

```
llmgw project list
llmgw project tool-prefix <name> on|off
```

`list` prints name, creation time, and prefix state. `tool-prefix` sets the flag
and reports a clear error for an unknown project. Register `project` in
`commandHandlers` and add it to the root usage text in
`internal/command/root.go`.

### Task 4.2 — Command tests

Following the existing command tests: `list` output shape, `tool-prefix` on and
off, unknown project, missing or malformed arguments, and the usage text.

### Task 4.3 — Integration: harness work and the end-to-end suite

This task owns the harness changes its assertions depend on. Two are needed
before a single assertion can be written:

- **The stub upstream must capture request bodies.** It records the
  authorization header and status today; add body capture in the same shape.
- **Assertions are stated in the format the stub receives.** The harness
  configures OpenAI-compatible and Codex providers, not an Anthropic-format one,
  so a `POST /v1/messages` is translated before it arrives: names land in
  `tools[].function.name`, `tool_choice.function.name`, and
  `messages[].tool_calls[].function.name`. Do not add an Anthropic provider to
  make the assertions prettier — that the prefix survives translation is the
  property worth proving.

Set the flag through `SetProjectToolPrefix`, not raw SQL, so the suite exercises
the store method rather than going around it.

The scenarios, from spec §11:

- Flag off: the stub receives `search_web`.
- Flag on: the stub receives `new_search_web` in the declarations, in the forced
  choice, and in the replayed history.
- Stub returns a tool call named `new_search_web`: the client sees `search_web`,
  non-streamed and streamed.
- Stub streams a tool call split across several writes: the client still sees
  one coherent `content_block_start` naming `search_web`.
- Stub returns a name without the prefix: the client sees it unchanged.
- `count_tokens` from a flagged project reports a larger `input_tokens` than the
  same request unflagged. Counting is answered locally and issues no upstream
  call, so the count is the only observable proof the rewrite reached it.
- A non-200 upstream response reaches the client unchanged.

Then the green gate: the full integration suite, the domain and adapter tests,
and `go vet`. This task commits only the fixes it surfaces.

Assert on shape and plausibility, never on model text.

**As built.** The non-200 scenario asserts something stronger than the plan
asked for. The SDK translates upstream errors into its own client-facing
envelope, so the literal upstream body is not guaranteed to survive and
asserting it verbatim would test the SDK rather than this feature. The test
instead drives the identical upstream failure with the flag off and on, and
asserts the client-visible status and body are byte-identical — which is the
property that matters. The failure is injected as `400 invalid_request_error`
rather than a 5xx: the SDK classifies 5xx and 429 as retryable, which would
trigger failover against the harness's two-account provider and consume a second
scripted response, making the test non-deterministic.
