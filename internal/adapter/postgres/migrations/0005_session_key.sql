-- Pivot the seed credential from an OAuth refresh token to a durable claude.ai session key.
-- session_key is the root credential (seeded once, never overwritten); the gateway bootstraps
-- access/refresh tokens from it on demand, so refresh_token becomes nullable (NULL until the first
-- bootstrap, and re-mintable from the session key when it later dies).
ALTER TABLE oauth_token ADD COLUMN session_key TEXT;
ALTER TABLE oauth_token ALTER COLUMN refresh_token DROP NOT NULL;
