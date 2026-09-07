-- Requests the gateway answered with 404 from its own router never reached a
-- handler, so they reached no provider and nothing could be billed for them.
-- They were nevertheless admitted as generations, produced no usage attempt,
-- and settled as unresolved accounting, which blocks every token and cost
-- budget of the project that sent them. Resolve them as the certain zero they
-- are, using the state an operator already resolves a known-zero request to.
--
-- Rows that reached a routed generation surface are excluded: there a missing
-- usage record is genuine uncertainty about what a provider charged, and it
-- must keep blocking. The excluded paths are the generation surfaces the
-- router exposed when this statement was written, and the list is frozen on
-- purpose: the statement runs once over rows already in the table, so it
-- cannot fall behind a router it never reads again.
--
-- Two deliberate ways this resolves less than the gateway now refuses, both
-- chosen because the two directions of error are not symmetric. Over-excluding
-- leaves a certain zero unresolved, and a rolling budget window is at most a
-- day wide, so such a row stops weighing on admission within a day and no new
-- one can be written once the router decides admission. Under-excluding clears
-- a request that did reach a handler, and that silently unblocks a budget on a
-- charge nobody can account for.
--
-- So the excluded surfaces are matched on path alone, ignoring method, even
-- though only some methods are registered on them: a method stated wrong here
-- would resolve a row that was served. And a recorded 404 is required as the
-- evidence that the gateway answered the request itself, which is why rows
-- recovered without any downstream status are left alone even when their path
-- was never routable.
UPDATE request_event r
SET accounting_state = 'resolved_zero',
    accounting_resolved_at = now()
WHERE r.operation = 'generation'
  AND r.state = 'completed'
  AND r.accounting_state IN ('accounting_unknown', 'pending')
  AND r.downstream_status = 404
  AND NOT EXISTS (SELECT 1 FROM usage_attempt a WHERE a.request_id = r.id)
  AND r.path NOT IN (
      '/v1/chat/completions',
      '/v1/completions',
      '/v1/messages',
      '/v1/responses',
      '/v1/responses/compact',
      '/v1/live',
      '/v1/alpha/search',
      '/v1/images/generations',
      '/v1/images/edits',
      '/v1/videos',
      '/v1/videos/generations',
      '/v1/videos/edits',
      '/v1/videos/extensions',
      '/v1beta/interactions'
  )
  AND r.path NOT LIKE '/v1beta/models/%'
  AND r.path NOT LIKE '/v1/live/%'
  AND r.path NOT LIKE '/v1/videos/%'
  AND r.path NOT LIKE '/v1/realtime%'
  AND r.path NOT LIKE '/openai/v1/%'
  AND r.path NOT LIKE '/backend-api/codex/%';
