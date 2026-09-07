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
