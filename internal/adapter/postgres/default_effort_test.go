package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestProjectDefaultEffort verifies the level's round trip through project
// creation, the store method that sets and clears it, and the client-key
// read path.
func TestProjectDefaultEffort(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)

	t.Run("fresh project key authenticates with no default level", func(t *testing.T) {
		created, err := store.CreateKey(ctx, "fresh-effort-project", "key", "pk-fresh-effort", make([]byte, 32), nil)
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if created.DefaultEffort != "" {
			t.Fatalf("CreateKey DefaultEffort = %q, want empty for a fresh project", created.DefaultEffort)
		}

		found, err := store.KeyByPublicID(ctx, "pk-fresh-effort")
		if err != nil {
			t.Fatalf("KeyByPublicID: %v", err)
		}
		if found.DefaultEffort != "" {
			t.Fatalf("KeyByPublicID DefaultEffort = %q, want empty for a fresh project", found.DefaultEffort)
		}
	})

	t.Run("SetProjectDefaultEffort sets an existing project and KeyByPublicID observes it", func(t *testing.T) {
		created, err := store.CreateKey(ctx, "set-effort-project", "key", "pk-set-effort", make([]byte, 32), nil)
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}

		if err := store.SetProjectDefaultEffort(ctx, "set-effort-project", "high"); err != nil {
			t.Fatalf("SetProjectDefaultEffort: %v", err)
		}

		found, err := store.KeyByPublicID(ctx, created.PublicID)
		if err != nil {
			t.Fatalf("KeyByPublicID: %v", err)
		}
		if found.DefaultEffort != "high" {
			t.Fatalf("KeyByPublicID DefaultEffort = %q, want %q", found.DefaultEffort, "high")
		}
	})

	t.Run("SetProjectDefaultEffort with the empty level clears it back to no default", func(t *testing.T) {
		if _, err := store.CreateKey(ctx, "clear-effort-project", "key", "pk-clear-effort", make([]byte, 32), nil); err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if err := store.SetProjectDefaultEffort(ctx, "clear-effort-project", "max"); err != nil {
			t.Fatalf("SetProjectDefaultEffort set: %v", err)
		}

		if err := store.SetProjectDefaultEffort(ctx, "clear-effort-project", ""); err != nil {
			t.Fatalf("SetProjectDefaultEffort clear: %v", err)
		}

		found, err := store.KeyByPublicID(ctx, "pk-clear-effort")
		if err != nil {
			t.Fatalf("KeyByPublicID: %v", err)
		}
		if found.DefaultEffort != "" {
			t.Fatalf("KeyByPublicID DefaultEffort = %q, want empty after clearing", found.DefaultEffort)
		}
	})

	t.Run("SetProjectDefaultEffort on an unknown project returns the sentinel and creates nothing", func(t *testing.T) {
		const unknown = "never-created-effort-project"

		err := store.SetProjectDefaultEffort(ctx, unknown, "low")
		if !errors.Is(err, ErrProjectNotFound) {
			t.Fatalf("SetProjectDefaultEffort error = %v, want ErrProjectNotFound", err)
		}

		exists, err := store.ProjectExists(ctx, unknown)
		if err != nil {
			t.Fatalf("ProjectExists: %v", err)
		}
		if exists {
			t.Fatalf("SetProjectDefaultEffort on unknown project %q created it", unknown)
		}
	})

	t.Run("Projects lists every project with its default-effort level", func(t *testing.T) {
		if _, err := store.CreateKey(ctx, "list-effort-none", "key", "pk-list-effort-none", make([]byte, 32), nil); err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if _, err := store.CreateKey(ctx, "list-effort-xhigh", "key", "pk-list-effort-xhigh", make([]byte, 32), nil); err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if err := store.SetProjectDefaultEffort(ctx, "list-effort-xhigh", "xhigh"); err != nil {
			t.Fatalf("SetProjectDefaultEffort: %v", err)
		}

		projects, err := store.Projects(ctx)
		if err != nil {
			t.Fatalf("Projects: %v", err)
		}
		levels := make(map[string]string, len(projects))
		for _, project := range projects {
			levels[project.Name] = project.DefaultEffort
		}
		if levels["list-effort-none"] != "" {
			t.Fatalf("project %q DefaultEffort = %q, want empty", "list-effort-none", levels["list-effort-none"])
		}
		if levels["list-effort-xhigh"] != "xhigh" {
			t.Fatalf("project %q DefaultEffort = %q, want %q", "list-effort-xhigh", levels["list-effort-xhigh"], "xhigh")
		}
	})

	t.Run("ListKeys and the rotate path survive the shared scanner change", func(t *testing.T) {
		created, err := store.CreateKey(
			ctx, "regression-effort-project", "original", "pk-regression-effort", make([]byte, 32), nil,
		)
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if err := store.SetProjectDefaultEffort(ctx, "regression-effort-project", "medium"); err != nil {
			t.Fatalf("SetProjectDefaultEffort: %v", err)
		}

		keys, err := store.ListKeys(ctx, "regression-effort-project")
		if err != nil {
			t.Fatalf("ListKeys: %v", err)
		}
		if len(keys) != 1 || keys[0].DefaultEffort != "medium" {
			t.Fatalf("ListKeys = %+v, want exactly the created key with DefaultEffort medium", keys)
		}

		rotated, err := store.RotateKey(
			ctx, created.ID, "pk-regression-effort-2", make([]byte, 32), time.Now().UTC(), 0,
		)
		if err != nil {
			t.Fatalf("RotateKey: %v", err)
		}
		if rotated.ProjectName != "regression-effort-project" || rotated.PublicID != "pk-regression-effort-2" {
			t.Fatalf("RotateKey = %+v, want the replacement under the same project", rotated)
		}
	})
}
