# Project Default Thinking Effort Design

**Date:** 2026-08-04
**Status:** Draft

## 1. Decision

A project can carry a default thinking effort. When it does, a client request
that names no effort of its own reaches Anthropic carrying the project's value
in `output_config.effort`. A client that names its own effort keeps it.

The setting is a per-project nullable level — `low`, `medium`, `high`, `xhigh`,
or `max` — unset by default, and it applies only to the Anthropic message
format.

## 2. Why

Anthropic's current models control reasoning depth through
`output_config.effort`, whose default is `high`. Two of the three models this
gateway serves bill and latency-charge that depth on every request, and a
client library that never sets the field silently takes the expensive default.

Setting it per project rather than per call means a project's cost and latency
profile is an operator decision, applied to every client it fronts, without any
client changing its integration. LLMGW already owns the request at that point,
so the value is injected where the gateway is, and the SDK stays unmodified.

## 3. Goals

- A project opts in; every other project is byte-for-byte unaffected, with no
  body read on its path.
- A client that states an effort always wins over the project default.
- The injection never turns a request that would have succeeded into an error.
- The rewrite consumes the SDK unmodified.

## 4. Non-Goals

- No default model. Choosing the model stays the client's.
- No `thinking` injection. On Fable 5, Opus 5 and Sonnet 5 — the generation this
  targets — omitting `thinking` already runs adaptive thinking, so the effort
  alone is what the depth depends on. Older models, where an omitted `thinking`
  means no thinking at all and an injected effort would be inert, are out of
  scope (see §10).
- No coverage of the OpenAI or Gemini client formats. Only the Anthropic message
  format carries the injection.
- No per-key override. The effort belongs to the project, like the tool-name
  prefix.

## 5. Trigger Conditions

The injection runs when all four hold:

1. The authenticated project has a `default_effort`.
2. The request path is `POST /v1/messages`.
3. The request passed authentication and admission.
4. The payload names neither `output_config.effort` nor
   `thinking.type: "disabled"`.

`POST /v1/messages/count_tokens` is excluded. Effort governs how many output
tokens the model spends thinking; it changes nothing about the input token
count, so injecting it there would alter a payload whose only answer is a number
it cannot move.

The fourth condition is what keeps the promise in §3. A field counts as named
when it is present, whatever its value: a client that sends an empty or
unrecognised effort has still expressed one, and replacing it would make the
project default an override on exactly the payloads hardest to reason about. An
explicit client effort is honoured because the project value is a default, not
an override. The
`thinking.type: "disabled"` case is sharper than politeness: on Opus 5,
disabled thinking is accepted only at effort `high` or below, so injecting
`xhigh` or `max` into a request that disabled thinking returns a `400` the
client would never have received on its own.

Requests from a project without the setting take exactly today's path. The body
is not read and no allocation is made on their behalf.

The injection runs after admission, like the tool-name rewrite: a request the
budget rules block never reaches the upstream, so rewriting its body is work
done for a response that is already decided.

## 6. Data Model and Operator Surface

`project` gains one nullable column:

```sql
ALTER TABLE project ADD COLUMN default_effort text;
```

as migration `0016_project_default_effort.sql`. `NULL` means the project is
unaffected, which is what makes the migration inert on every existing row.

`KeyByPublicID` already selects `prefix_tool_names` from the joined `project`,
so the column joins that `SELECT` and is carried on `governance.ClientKey`, then
onto `governance.KeyIdentity` as `DefaultEffort`. The middleware reads it from
the identity it already holds in `admit`; like `PrefixToolNames` it is not added
to `RequestIdentity` and does not enter the request context, because the only
consumer is the middleware itself.

The `project` command gains one verb:

```
llmgw project effort <name> low|medium|high|xhigh|max|none
```

`none` clears the column. Like `tool-prefix`, it fails on an unknown project
rather than creating one, and `project list` prints the value alongside the
prefix state, with `none` for `NULL`.

The level enumeration is validated in the CLI, which is the only write path. No
`CHECK` constraint is added: it would guard only against an operator editing
their own database by hand, and an unrecognised value is refused upstream
anyway.

## 7. Payload Injection

The injection sets one field:

```
output_config.effort = <the project's level>
```

Nothing else in the payload is read or written. `thinking` is left exactly as
the client sent it, present or absent.

A payload that is not valid JSON is passed through untouched, for the same
reason the tool-name rewrite does it: rejecting it here would replace
Anthropic's specific validation error with a vaguer one from us. A payload whose
`output_config` exists but holds no `effort` gains the field beside whatever
else is there.

## 8. Code Layout

Today `rewriteToolNames` reads the request body, applies the tool-name rewrite,
and installs the result. A second, independent rewrite would read the body a
second time for a project that enables both. The middleware's rewrite entry
point is therefore generalized: it reads the body once, applies whichever
transformations the identity and route call for, and installs the result once.
The bounds are the ones already in place — the 32 MiB ceiling with `413`, the
`400` on a body that cannot be read, and the untouched passthrough of invalid
JSON — and they now cover both transformations rather than the tool-name rewrite
alone.

The injection itself is a pure function over `[]byte` in the domain, taking the
payload and the level and returning the payload, with no HTTP, SQL, or SDK
import. It is the unit under test for every injection rule.

Persistence follows the existing shape: the column in the `KeyByPublicID` query
and the scan, the value on the domain types, and a store method for the CLI to
set it.

## 9. Error Handling

The injection never fails a request that would otherwise succeed. Malformed JSON
is forwarded unchanged, and a failed write leaves the payload as it was.

No new rejection is introduced. The `413` and `400` on the flagged path already
exist for the tool-name rewrite and now cover a project that enables only this
setting.

If the project value cannot be read, that is an authentication failure, and it
already produces `503` through the existing path.

## 10. Testing

The integration suite drives the real gateway over HTTP against a stub upstream,
which captures request bodies since the tool-prefix feature. The harness gains
one Anthropic-format provider — a `claude-api-key` entry pointed at the stub —
and these tests run against it. On that path the executor forwards the payload
essentially untouched, so the assertion is on the literal `output_config.effort`
the stub receives.

The harness's existing providers cannot carry these assertions, and the reason
is worth recording. A `POST /v1/messages` routed to the OpenAI-compatible
provider is translated by a converter that reads `output_config.effort` only
when a `thinking` object is present — and §4 rules out injecting one, so the
effort would vanish before the stub. Giving the fixture its own `thinking` to
make it visible breaks the opposite assertion instead, because that converter
then writes a default `reasoning_effort` for the project that set none. The
Codex provider writes a default `reasoning.effort` unconditionally. No single
fixture makes all the cases below true, and the Anthropic-format path is the one
the feature actually targets.

- A project with no default sends a request naming no effort; the stub sees a
  payload carrying no `output_config.effort`.
- A project with a default sends the same request; the stub sees the project's
  level.
- A project with a default sends a request naming its own effort; the stub sees
  the client's level.
- A project with a default sends a request disabling thinking; the stub sees no
  injected effort.
- A project with both this setting and the tool-name prefix sends one request;
  the stub sees both the namespaced tool names and the injected effort, which is
  what proves the single body read applies both transformations.

The `count_tokens` exclusion is not asserted through the stub. Token counting is
answered locally by the executor and issues no upstream call, and the injected
field would not move the reported count either, so the route decision has no
observable consequence to assert end to end. It is covered where it is decided,
by an adapter test on the eligibility predicate, alongside the existing
tool-prefix route tests.

Domain tests cover the pure function directly: an absent `output_config`, an
`output_config` present without `effort`, an `effort` already set, a payload
disabling thinking, and malformed JSON.

Assertions are on shape and plausibility, never on model text.

## 11. Known Limitations

- **Inert on pre-4.6-generation models.** On Opus 4.8, Opus 4.7 and Sonnet 4.6,
  an omitted `thinking` means no thinking runs, and an injected effort changes
  nothing. The setting is silently without effect for a project pinned to one of
  those models. This is accepted: the deployment targets Fable 5, Opus 5 and
  Sonnet 5, where an omitted `thinking` already runs adaptive.
- **Dropped when a tool choice is forced.** The SDK deletes
  `output_config.effort` when `tool_choice` is `any` or a specific tool, because
  Anthropic refuses thinking controls there. The injected value is discarded on
  those requests, silently and correctly.
- **Translated on a non-Anthropic upstream.** If the credential pool routes an
  Anthropic-format request to a Gemini or Codex provider, the SDK converts the
  injected effort into that provider's own reasoning control. That conversion is
  the SDK's to own, and the value reaching the upstream is not the string we
  wrote.
- **No validation against the model.** A level Anthropic stops accepting, or one
  a given model does not support, is injected as written and refused upstream.
  LLMGW does not pre-empt this.

## 12. Deployment Notes

- Migration `0016` runs at startup like every other; it adds a nullable column
  and needs no backfill.
- No configuration change. The feature is inert until an operator runs
  `llmgw project effort <name> <level>`.
- No new environment variable, secret, or service ordering constraint.
