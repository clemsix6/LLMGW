package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

var (
	// Confirm Store implements the project budget repository port.
	_ governance.BudgetRepository = (*Store)(nil)

	errInvalidBudgetMaximum = errors.New("invalid budget maximum")
)

// SetBudget creates or replaces one project budget limit.
func (s *Store) SetBudget(
	ctx context.Context,
	project string,
	dimension governance.Dimension,
	window governance.Window,
	maximum float64,
	action governance.Action,
) (governance.BudgetLimit, error) {
	if err := validateBudgetMaximum(dimension, maximum); err != nil {
		return governance.BudgetLimit{}, err
	}

	const query = `
INSERT INTO budget_limit (project_id, dimension, "window", max_value, action)
SELECT id, $2, $3, $4, $5 FROM project WHERE name = $1
ON CONFLICT (project_id, dimension, "window", action) DO UPDATE SET
    max_value = EXCLUDED.max_value,
    updated_at = now()
RETURNING id, project_id, dimension, "window", max_value, action, created_at, updated_at`
	limit, err := scanBudgetLimit(
		s.pool.QueryRow(ctx, query, project, dimension, window, maximum, action),
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return governance.BudgetLimit{}, fmt.Errorf(
			"set budget for project %q:\n%w",
			project,
			pgx.ErrNoRows,
		)
	}
	if err != nil {
		return governance.BudgetLimit{}, fmt.Errorf("set budget for project %q:\n%w", project, err)
	}
	return limit, nil
}

// ListBudgets returns the limits belonging only to the exact named project.
func (s *Store) ListBudgets(
	ctx context.Context,
	project string,
) ([]governance.BudgetLimit, error) {
	const query = `
SELECT b.id, b.project_id, b.dimension, b."window", b.max_value, b.action,
       b.created_at, b.updated_at
FROM budget_limit b
JOIN project p ON p.id = b.project_id
WHERE ($1 = '' OR p.name = $1)
ORDER BY b.id`
	rows, err := s.pool.Query(ctx, query, project)
	if err != nil {
		return nil, fmt.Errorf("list budgets for project %q:\n%w", project, err)
	}
	defer rows.Close()

	var limits []governance.BudgetLimit
	for rows.Next() {
		limit, err := scanBudgetLimit(rows)
		if err != nil {
			return nil, fmt.Errorf("scan budget for project %q:\n%w", project, err)
		}
		limits = append(limits, limit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate budgets for project %q:\n%w", project, err)
	}
	return limits, nil
}

// DeleteBudget removes only the budget row with the exact database identifier.
func (s *Store) DeleteBudget(ctx context.Context, budgetID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM budget_limit WHERE id = $1`, budgetID)
	if err != nil {
		return fmt.Errorf("delete budget %d:\n%w", budgetID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("delete budget %d:\n%w", budgetID, pgx.ErrNoRows)
	}
	return nil
}

// validateBudgetMaximum rejects unsafe values and fractional count maxima.
func validateBudgetMaximum(dimension governance.Dimension, maximum float64) error {
	if math.IsNaN(maximum) || math.IsInf(maximum, 0) || maximum < 0 {
		return fmt.Errorf("%w: maximum must be finite and non-negative", errInvalidBudgetMaximum)
	}
	if (dimension == governance.DimensionCalls || dimension == governance.DimensionTokens) &&
		maximum != math.Trunc(maximum) {
		return fmt.Errorf("%w: %s maximum must be an integer", errInvalidBudgetMaximum, dimension)
	}
	return nil
}

// scanBudgetLimit reads the common budget limit projection.
func scanBudgetLimit(row pgx.Row) (governance.BudgetLimit, error) {
	var limit governance.BudgetLimit
	err := row.Scan(
		&limit.ID,
		&limit.ProjectID,
		&limit.Dimension,
		&limit.Window,
		&limit.MaxValue,
		&limit.Action,
		&limit.CreatedAt,
		&limit.UpdatedAt,
	)
	return limit, err
}
