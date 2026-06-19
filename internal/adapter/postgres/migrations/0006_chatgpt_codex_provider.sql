-- Extend the provider.type CHECK constraint to allow chatgpt_codex_oauth, then seed the
-- chatgpt-codex provider row so oauth_token rows can reference it. ON CONFLICT keeps the
-- migration idempotent if the row was inserted by hand before this ran.
ALTER TABLE provider DROP CONSTRAINT provider_type_check;
ALTER TABLE provider ADD CONSTRAINT provider_type_check
    CHECK (type IN ('claude_max_oauth', 'anthropic_api', 'openrouter', 'chatgpt_codex_oauth'));
INSERT INTO provider (name, type) VALUES ('chatgpt-codex', 'chatgpt_codex_oauth')
ON CONFLICT (name) DO NOTHING;
