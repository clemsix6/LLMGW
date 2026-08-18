-- Lets a project opt into refusing non-streamed generation responses whose
-- tool-call inputs carry leaked function-call markup, turning them into a
-- retryable upstream error instead of a corrupted success. DEFAULT false
-- keeps every existing row inert.

ALTER TABLE project ADD COLUMN reject_tool_markup boolean NOT NULL DEFAULT false;
