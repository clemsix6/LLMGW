package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// ErrProjectNotFound is returned when an operator command targets a project that
// key create has not yet made. Setting the tool-name-prefix flag never creates one:
// implicit project creation stays a property of key create alone.
var ErrProjectNotFound = errors.New("project not found")

// Projects returns every project ordered by name.
func (s *Store) Projects(ctx context.Context) ([]governance.Project, error) {
	const query = `
SELECT id, name, created_at, prefix_tool_names, COALESCE(default_effort, '')
FROM project
ORDER BY name`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query projects:\n%w", err)
	}
	defer rows.Close()

	var projects []governance.Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project:\n%w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects:\n%w", err)
	}
	return projects, nil
}

// SetProjectToolPrefix enables or disables outbound tool-name prefixing for an
// already-created project. It fails with ErrProjectNotFound rather than creating
// the project, so the flag can never be the thing that brings a project into being.
func (s *Store) SetProjectToolPrefix(ctx context.Context, name string, enabled bool) error {
	const query = `UPDATE project SET prefix_tool_names = $2 WHERE name = $1`

	tag, err := s.pool.Exec(ctx, query, name, enabled)
	if err != nil {
		return fmt.Errorf("set tool-name prefix for project %q:\n%w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set tool-name prefix for project %q:\n%w", name, ErrProjectNotFound)
	}
	return nil
}

// SetProjectDefaultEffort sets or clears an already-created project's default
// Anthropic thinking effort. An empty level writes NULL, which is what the
// injection reads as "no default". It fails with ErrProjectNotFound rather
// than creating the project, so the setting can never be the thing that
// brings a project into being.
func (s *Store) SetProjectDefaultEffort(ctx context.Context, name string, level string) error {
	const query = `UPDATE project SET default_effort = NULLIF($2, '') WHERE name = $1`

	tag, err := s.pool.Exec(ctx, query, name, level)
	if err != nil {
		return fmt.Errorf("set default effort for project %q:\n%w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set default effort for project %q:\n%w", name, ErrProjectNotFound)
	}
	return nil
}

// scanProject scans one row of the project projection.
func scanProject(row pgx.Row) (governance.Project, error) {
	var project governance.Project
	err := row.Scan(
		&project.ID, &project.Name, &project.CreatedAt, &project.PrefixToolNames, &project.DefaultEffort,
	)
	return project, err
}
