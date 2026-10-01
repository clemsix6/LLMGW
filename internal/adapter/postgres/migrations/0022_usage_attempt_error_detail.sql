-- Records the error type and message of 4xx failures so a rejected request can
-- be diagnosed afterwards. Both columns are nullable and additive: a row
-- without a standard error body keeps them NULL, so no existing row changes.

ALTER TABLE usage_attempt ADD COLUMN upstream_error_type text;
ALTER TABLE usage_attempt ADD COLUMN upstream_error_message text;
