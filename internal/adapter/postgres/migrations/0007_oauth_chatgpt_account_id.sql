-- Add chatgpt_account_id to oauth_token to store the ChatGPT account identifier used by
-- the Codex OAuth flow. The column is nullable: Claude Max rows leave it NULL.
ALTER TABLE oauth_token ADD COLUMN chatgpt_account_id TEXT;
