-- Seed the single V1 Claude Max provider so oauth_token rows have a stable provider_id
-- to reference. Token operations resolve this row read-only (no write side-effect).
INSERT INTO provider (name, type) VALUES ('claude_max', 'claude_max_oauth') ON CONFLICT (name) DO NOTHING;
