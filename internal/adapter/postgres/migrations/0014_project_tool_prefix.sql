-- Lets a project opt into having every tool name it declares renamed on the
-- way to Anthropic and renamed back on the way to the client (design
-- docs/superpowers/specs/2026-08-04-project-tool-name-prefix-design.md).
-- Off by default: existing projects are unaffected until an operator runs
-- `llmgw project tool-prefix <name> on`.

ALTER TABLE project ADD COLUMN prefix_tool_names boolean NOT NULL DEFAULT false;
