-- Drops the per-project tool-name-prefix opt-in added by 0014. The rewrite is
-- now unconditional and MCP-shaped, so nothing reads the column any more and
-- no project can turn it off (design
-- docs/superpowers/specs/2026-08-04-project-tool-name-prefix-design.md).

ALTER TABLE project DROP COLUMN prefix_tool_names;
