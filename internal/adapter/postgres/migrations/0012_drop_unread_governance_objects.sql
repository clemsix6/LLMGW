-- Drops the schema left behind by the pre-SDK gateway.
--
-- Each object below is written by no statement and read by no statement in
-- the gateway. They survived the CLIProxyAPI pivot only because migrations
-- are append-only: 0010 replaced the homemade accounting tables without
-- removing what the earlier design had introduced.
--
-- The migrations that created them stay untouched, so a fresh database still
-- replays the same history and reaches the same state as an existing one.

-- The homemade router's route table, superseded by RouteClass in Go.
DROP TABLE IF EXISTS route;

-- The pre-pivot usage table, renamed by 0010 and never read since.
DROP TABLE IF EXISTS legacy_usage_event;

-- Provider credential columns the SDK now owns: it tracks cooldowns in its
-- own auth directory, and session-key authentication is gone.
ALTER TABLE oauth_token
    DROP COLUMN IF EXISTS cooldown_until,
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS session_key;

-- Provider configuration the SDK reads from the shared YAML instead.
ALTER TABLE provider
    DROP COLUMN IF EXISTS config_json,
    DROP COLUMN IF EXISTS enabled;

-- Never selected; project creation time is not reported anywhere.
ALTER TABLE project
    DROP COLUMN IF EXISTS created_at;
