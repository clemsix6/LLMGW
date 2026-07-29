package cliproxy

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestServiceNaturalReturnWaitsForQueuedSDKUsagePersistence catches a natural
// SDK return closing its real usage manager queue without waiting for the real
// PostgreSQL callback and authenticated barrier to become durable.
func TestServiceNaturalReturnWaitsForQueuedSDKUsagePersistence(t *testing.T) {
	ctx := context.Background()
	dsn := naturalReturnPostgres(t, ctx)
	store, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open governance store: %v", err)
	}
	t.Cleanup(store.Close)
	observer, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open observer pool: %v", err)
	}
	t.Cleanup(observer.Close)

	key, err := store.CreateKey(
		ctx,
		"natural-return",
		"primary",
		"MDEyMzQ1Njc4OWFi",
		make([]byte, 32),
		nil,
	)
	if err != nil {
		t.Fatalf("create project key: %v", err)
	}
	requestedAt := time.Date(2030, 7, 27, 11, 59, 0, 0, time.UTC)
	requestID := uuid.NewString()
	model := "test-model"
	admission, err := store.AdmitGeneration(ctx, governance.RequestEvent{
		ID:              requestID,
		ProjectID:       key.ProjectID,
		ClientKeyID:     key.ID,
		Operation:       governance.OperationGeneration,
		RequestedAt:     requestedAt,
		Method:          "POST",
		Path:            "/v1/chat/completions",
		RequestedModel:  &model,
		State:           governance.RequestInFlight,
		AccountingState: governance.AccountingPending,
	}, requestedAt)
	if err != nil || !admission.Allowed {
		t.Fatalf("admit generation = (%#v, %v)", admission, err)
	}

	bridge, err := NewUsageBridge(rand.Reader, 1)
	if err != nil {
		t.Fatalf("construct usage bridge: %v", err)
	}
	if !bridge.reserve(requestID) {
		t.Fatal("reserve usage group")
	}
	manager := sdkusage.NewManager(1)
	manager.Register(NewUsagePlugin(store, bridge, postgres.IsTransientUsageError))
	manager.Start(context.Background())
	t.Cleanup(manager.Stop)
	bridge.publishRecord = manager.Publish

	held, err := observer.Begin(ctx)
	if err != nil {
		t.Fatalf("begin parent lock: %v", err)
	}
	t.Cleanup(func() { _ = held.Rollback(context.Background()) })
	if _, err := held.Exec(ctx, `SELECT id FROM request_event WHERE id = $1 FOR UPDATE`, requestID); err != nil {
		t.Fatalf("lock request parent: %v", err)
	}

	principal, ok := bridge.principal(RequestIdentity{
		RequestID:   requestID,
		KeyPublicID: key.PublicID,
	})
	if !ok {
		t.Fatal("construct usage principal")
	}
	manager.Publish(context.Background(), sdkusage.Record{
		Provider:     "openai-compatible-integration",
		ExecutorType: "OpenAICompatExecutor",
		Model:        "upstream-model",
		Alias:        model,
		APIKey:       principal,
		AuthID:       "account-a",
		AuthType:     "api-key",
		RequestedAt:  requestedAt,
		Detail:       sdkusage.Detail{InputTokens: 2, OutputTokens: 1, TotalTokens: 3},
	})
	bridge.publishBarrier(requestID)
	awaitNaturalReturnUsageLock(t, observer)

	returnSDK := make(chan struct{})
	sdkErr := errors.New("listener stopped")
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run: func(context.Context) error {
			<-returnSDK
			manager.Stop()
			return sdkErr
		},
		shutdown: func(context.Context) error { return nil },
	}
	service := newLifecycleService(proxy, closedSignal(), nil, 2*time.Second)
	service.usageBridge = bridge
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(context.Background()) }()
	<-proxy.runEntered
	close(returnSDK)

	assertRunStillBlocked(t, runDone)
	if err := held.Rollback(ctx); err != nil {
		t.Fatalf("release request parent: %v", err)
	}
	if err := <-runDone; !errors.Is(err, sdkErr) {
		t.Fatalf("natural service return = %v, want SDK cause", err)
	}

	var attempts int
	var storedCreatedAt time.Time
	if err := observer.QueryRow(
		ctx,
		`SELECT count(*), min(created_at) FROM usage_attempt WHERE request_id = $1`,
		requestID,
	).Scan(&attempts, &storedCreatedAt); err != nil {
		t.Fatalf("query persisted usage: %v", err)
	}
	if attempts != 1 || !storedCreatedAt.Equal(requestedAt) || bridge.outstanding() != 0 {
		t.Fatalf(
			"natural-return persistence = attempts:%d created:%s outstanding:%d",
			attempts,
			storedCreatedAt,
			bridge.outstanding(),
		)
	}
}

func naturalReturnPostgres(t *testing.T, ctx context.Context) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	container, err := tcpostgres.Run(
		ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("llmgw"),
		tcpostgres.WithUsername("llmgw"),
		tcpostgres.WithPassword("llmgw"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate PostgreSQL: %v", err)
		}
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("PostgreSQL connection string: %v", err)
	}
	return dsn
}

func awaitNaturalReturnUsageLock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity
    WHERE wait_event_type = 'Lock'
      AND query LIKE '%FOR KEY SHARE OF r, ck%'
)`).Scan(&blocked)
		if err != nil {
			t.Fatalf("inspect usage callback lock: %v", err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("usage callback did not block on the real request parent")
}
