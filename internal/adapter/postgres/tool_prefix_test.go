package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestProjectToolPrefix verifies the flag's round trip through project
// creation, the store methods that operate it, and the client-key read path.
//
// scanClientKey is shared by KeyByPublicID, KeyByID, ListKeys, and
// lockedClientKey (used by RotateKey): adding a destination to the scanner
// while updating only some of the four projections compiles cleanly and then
// fails at runtime with a pgx destination-count mismatch. The last subtest
// exercises ListKeys and RotateKey specifically to guard against that
// regression.
func TestProjectToolPrefix(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)

	t.Run("fresh project key authenticates with the flag off", func(t *testing.T) {
		created, err := store.CreateKey(ctx, "fresh-prefix-project", "key", "pk-fresh-prefix", make([]byte, 32), nil)
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if created.PrefixToolNames {
			t.Fatalf("CreateKey PrefixToolNames = true, want false for a fresh project")
		}

		found, err := store.KeyByPublicID(ctx, "pk-fresh-prefix")
		if err != nil {
			t.Fatalf("KeyByPublicID: %v", err)
		}
		if found.PrefixToolNames {
			t.Fatalf("KeyByPublicID PrefixToolNames = true, want false for a fresh project")
		}
	})

	t.Run("SetProjectToolPrefix flips an existing project and KeyByPublicID observes it", func(t *testing.T) {
		created, err := store.CreateKey(ctx, "toggle-prefix-project", "key", "pk-toggle-prefix", make([]byte, 32), nil)
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}

		if err := store.SetProjectToolPrefix(ctx, "toggle-prefix-project", true); err != nil {
			t.Fatalf("SetProjectToolPrefix: %v", err)
		}

		found, err := store.KeyByPublicID(ctx, created.PublicID)
		if err != nil {
			t.Fatalf("KeyByPublicID: %v", err)
		}
		if !found.PrefixToolNames {
			t.Fatalf("KeyByPublicID PrefixToolNames = false after enabling, want true")
		}
	})

	t.Run("SetProjectToolPrefix on an unknown project returns the sentinel and creates nothing", func(t *testing.T) {
		const unknown = "never-created-prefix-project"

		err := store.SetProjectToolPrefix(ctx, unknown, true)
		if !errors.Is(err, ErrProjectNotFound) {
			t.Fatalf("SetProjectToolPrefix error = %v, want ErrProjectNotFound", err)
		}

		exists, err := store.ProjectExists(ctx, unknown)
		if err != nil {
			t.Fatalf("ProjectExists: %v", err)
		}
		if exists {
			t.Fatalf("SetProjectToolPrefix on unknown project %q created it", unknown)
		}
	})

	t.Run("Projects lists every project with its flag state", func(t *testing.T) {
		if _, err := store.CreateKey(ctx, "list-prefix-off", "key", "pk-list-prefix-off", make([]byte, 32), nil); err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if _, err := store.CreateKey(ctx, "list-prefix-on", "key", "pk-list-prefix-on", make([]byte, 32), nil); err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if err := store.SetProjectToolPrefix(ctx, "list-prefix-on", true); err != nil {
			t.Fatalf("SetProjectToolPrefix: %v", err)
		}

		projects, err := store.Projects(ctx)
		if err != nil {
			t.Fatalf("Projects: %v", err)
		}
		states := make(map[string]bool, len(projects))
		for _, project := range projects {
			states[project.Name] = project.PrefixToolNames
		}
		if states["list-prefix-off"] {
			t.Fatalf("project %q reports PrefixToolNames = true, want false", "list-prefix-off")
		}
		if !states["list-prefix-on"] {
			t.Fatalf("project %q reports PrefixToolNames = false, want true", "list-prefix-on")
		}
	})

	t.Run("ListKeys and the rotate path survive the shared scanner change", func(t *testing.T) {
		created, err := store.CreateKey(
			ctx, "regression-prefix-project", "original", "pk-regression-prefix", make([]byte, 32), nil,
		)
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}

		keys, err := store.ListKeys(ctx, "regression-prefix-project")
		if err != nil {
			t.Fatalf("ListKeys: %v", err)
		}
		if len(keys) != 1 || keys[0].PublicID != created.PublicID {
			t.Fatalf("ListKeys = %+v, want exactly the created key", keys)
		}

		rotated, err := store.RotateKey(
			ctx, created.ID, "pk-regression-prefix-2", make([]byte, 32), time.Now().UTC(), 0,
		)
		if err != nil {
			t.Fatalf("RotateKey: %v", err)
		}
		if rotated.ProjectName != "regression-prefix-project" || rotated.PublicID != "pk-regression-prefix-2" {
			t.Fatalf("RotateKey = %+v, want the replacement under the same project", rotated)
		}
	})
}
