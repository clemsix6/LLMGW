# Project Default Thinking Effort — Implementation Plan

**Spec:** `docs/superpowers/specs/2026-08-04-project-default-effort-design.md`
**Date:** 2026-08-04

Single phase. Everything the spec describes is implemented in this iteration.

## Cross-cutting constraints

These bind every task; a task that violates one is wrong even if its own tests
pass.

- **Rewrite JSON with `gjson`/`sjson`, never `encoding/json`.** Both are already
  direct requires. Round-tripping through `map[string]any` would reorder keys and
  mangle number precision on a payload that must otherwise reach Anthropic
  unchanged.
- **The level is a `string`, empty meaning "no default".** The column is
  `text NULL`, and **every projection scanned into a Go `string` reads it through
  `COALESCE(p.default_effort, '')`** — the rule is about projections, not about
  the word `SELECT`, since an `INSERT … RETURNING` scans just the same and pgx
  refuses a `NULL` into a `*string`. With the `COALESCE`, nothing downstream
  carries a pointer or a `sql.Null*`. An empty string is never a valid level, so
  the sentinel is unambiguous.
- **No `replace` directive** ever reaches a commit.
- Every exported symbol carries a docstring; every struct field an inline
  comment. Functions stay under 30 lines, files under 400.
- The domain package imports no HTTP, SQL, or SDK package.
- Run `just` recipes for build and test; do not invent new ones.

---

## Batch 1 — The level, from the column to the authenticated identity

### Task 1.1 — Persist and propagate the level

Add `internal/adapter/postgres/migrations/0016_project_default_effort.sql`:

```sql
ALTER TABLE project ADD COLUMN default_effort text;
```

Then carry it through the read path, mirroring `prefix_tool_names` exactly:

- `governance.Project`, `governance.ClientKey` and `governance.KeyIdentity` each
  gain `DefaultEffort string`.
- `keyIdentity` in `internal/domain/projectkey/service.go:192` copies it onto the
  identity it builds.

**`scanClientKey` is shared — all four of its projections must be updated
together.** `internal/adapter/postgres/keys.go` scans it for `KeyByPublicID`
(line 52), `KeyByID` (71), `ListKeys` (123) and `lockedClientKey` (249). Adding a
destination to the scanner while updating only some projections compiles cleanly
and then fails at runtime with a pgx destination-count mismatch — missing
`ListKeys` breaks `llmgw key list` and nothing else, which is why it is the one
easiest to skip. Each of those four `SELECT`s gains `COALESCE(p.default_effort,
'')`, in the same position as the new scan destination in `scanClientKey`. The
fifth `SELECT` at line 169 feeds `scanKeyInfo`, not `scanClientKey`, and is left
alone.

**The write path is deliberately left alone.** `ensureKeyProject` and
`insertClientKey` resolve `prefix_tool_names` so that the `ClientKey` returned by
`CreateKey` and `RotateKey` carries it — but nothing reads that field on that
path: `keyInfo` drops it, and `keyIdentity` is built from `KeyByPublicID` alone.
Threading a second value through would change three signatures and touch the
key-rotation transaction to populate a field with no reader. So `CreateKey` and
`RotateKey` return an empty `DefaultEffort` regardless of the project's real
state, and that is correct rather than tolerated. It also keeps the `RETURNING`
in `ensureKeyProject` — an unguarded scan into a `string` — away from the new
nullable column.

### Task 1.2 — The domain level and the store write

Create `internal/domain/effort` with the level rule and nothing else yet:

- The five valid levels (`low`, `medium`, `high`, `xhigh`, `max`) and a
  `ParseLevel(string) (string, bool)` that accepts them plus `none`, which maps
  to the empty string. It is the single definition of what a level is; the CLI
  does not carry its own list.

Add `SetProjectDefaultEffort(ctx, name, level string) error` to the store in
`internal/adapter/postgres/projects.go`, writing `NULL` for the empty string, and
failing with `ErrProjectNotFound` on zero rows affected rather than creating the
project — implicit creation stays a property of `key create` alone, exactly as
`SetProjectToolPrefix` does. Extend the `Projects` query and `scanProject` with
the `COALESCE`d column.

### Task 1.3 — Operator surface (integration)

Wire the batch into the composition root so it is reachable:

- `internal/command/project.go` gains the `effort` verb:
  `llmgw project effort <name> low|medium|high|xhigh|max|none`, parsing through
  `effort.ParseLevel`, failing with a clear message on an unknown project, and
  printing the resulting state the way `tool-prefix` does.
- `printProject` gains the level, printing `none` for the empty string.
- The usage line lists the new verb.

Cover it: adapter tests for the store write and the read-back through
`KeyByPublicID` (including that a project without a level yields an empty
string), and command tests for the verb — accepted levels, `none`, a rejected
level, and an unknown project.

---

## Batch 2 — Injection into the outbound payload

### Task 2.1 — The pure injection

Add to `internal/domain/effort` the function the adapter calls:
`Apply(payload []byte, level string) []byte`, which sets
`output_config.effort` to the level and touches nothing else.

It returns the payload unchanged when: the payload is not valid JSON, the level
is empty, `output_config.effort` already exists (whatever its value — presence is
what counts, per spec §5), or `thinking.type` equals `disabled`. A failed
`sjson` write also returns the original: an injection never fails a request that
would otherwise succeed.

Unit tests cover each of those branches plus the two writing cases — an absent
`output_config`, and an `output_config` present without `effort`, which must keep
its sibling fields.

### Task 2.2 — Single-read body rewrite in the middleware — **complex, dispatch on opus**

Today `rewriteToolNames` in `internal/adapter/cliproxy/tool_prefix.go` owns the
read-transform-install cycle for one transformation. Generalize it so the body is
read once and every applicable transformation is applied to it, in a new
`internal/adapter/cliproxy/request_rewrite.go`:

- A small unexported value describing what this request needs — whether to
  prefix tool names, and which level to inject — resolved from the
  `governance.KeyIdentity` and the route.
- The tool-name rewrite keeps its route set (`/v1/messages` and
  `/v1/messages/count_tokens`); the effort injection is `/v1/messages` only, per
  spec §5. A request needing neither reads no body at all, which is the property
  that keeps unaffected projects on today's path.
- `readBoundedBody` and `installRequestBody` in `tool_prefix_request.go` are
  reused as-is, so the 32 MiB ceiling with `413` and the `400` on an unreadable
  body now cover both transformations. Move them to the new file only if that
  leaves both files coherent.

**Replace the call site, do not merely add beside it.** `middleware.go:194`
calls `m.rewriteToolNames(c, keyIdentity, request.ID, reserved)` and is its only
caller. That line becomes the call to the generalized entry point, and the old
one is deleted. Leaving `admit` calling `rewriteToolNames` compiles, passes the
tool-prefix tests, and ships an effort injection nothing reaches.

Two invariants of the code being replaced are easy to drop silently, and both
must survive: `rewriteRequestBody` returns early when `c.Request.Body == nil`
(`tool_prefix_request.go:25`), and `readBoundedBody` **has already called
`abortSafe`** when it returns false — the replacement must not write a second
response over it.

**The generation permit must still be released on every refusal path.** The
current `rewriteToolNames` returns before the barrier defer is registered, so it
calls `m.bridge.release(requestID)` itself when the rewrite refuses the request;
the replacement returns `false` on the same paths and must release identically.
Losing that leaks a permit and permanently shrinks the bridge's capacity for the
life of the process.

`installToolPrefixWriter` and the response wrapper are untouched: nothing in a
response carries the effort.

### Task 2.3 — End-to-end coverage (integration) — **complex, dispatch on opus**

Integration tests in `test/integration`, against the stub upstream that already
captures request bodies.

**The harness needs an Anthropic-format provider first.** Its `configYAML` in
`harness_test.go` declares only `openai-compatibility` and `codex-api-key`, and
neither makes the injected effort observable — spec §10 records why. Add a
`claude-api-key` entry pointed at the stub, with its own model alias, following
the shape of the entries already there (`api-key`, `base-url`, `models` with
`name`/`alias`/`force-mapping`). Requests in this task use that alias; every
existing test keeps its own. The stub's scripted response for these must be an
Anthropic message payload rather than the OpenAI-shaped `defaultStubResponse`,
since the executor no longer translates the response.

On that path the executor forwards the payload essentially untouched, so assert
on the literal `output_config.effort` in the captured body.

- A project with no level: the stub sees no `output_config.effort`.
- A project with a level: the stub sees it.
- A project with a level, client sending its own effort: the client's wins.
- A project with a level, client disabling thinking: nothing is injected.
- A project with both this level and the tool-name prefix, one request: the stub
  sees the namespaced tool names *and* the level, which is what proves the single
  body read applies both transformations.

The `count_tokens` exclusion is asserted at the adapter, on the eligibility
predicate, alongside the existing tool-prefix route tests — token counting is
answered locally and issues no upstream call, so it has no end-to-end
observable.

Then run the full suite plus `go vet`. This task commits only the fixes it
surfaces; if everything is green its artifact is the green gate.
