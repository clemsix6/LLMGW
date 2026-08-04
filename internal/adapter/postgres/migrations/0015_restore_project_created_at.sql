-- Restores project.created_at, which 0012 dropped because nothing read it at
-- the time ("project creation time is not reported anywhere").
--
-- The new `llmgw project list` command (design
-- docs/superpowers/specs/2026-08-04-project-tool-name-prefix-design.md §6)
-- reports each project's creation time alongside its tool-name-prefix state,
-- so it is read again. There is nothing to backfill the original value from
-- — 0012 discarded it — so an existing project's apparent creation time
-- becomes the moment this migration runs.

ALTER TABLE project ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
