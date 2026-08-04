-- Lets a project opt into a default Anthropic thinking effort injected on every
-- outbound request that names none of its own (design
-- docs/superpowers/specs/2026-08-04-project-default-effort-design.md).
-- NULL means the project is unaffected, which is what makes the migration
-- inert on every existing row.

ALTER TABLE project ADD COLUMN default_effort text;
