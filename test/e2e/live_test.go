package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/command"
	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/projectkey"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

type liveProjection struct {
	LLMGW struct {
		Live liveSettings `yaml:"live"`
	} `yaml:"llmgw"`
}

type liveProtocolSettings struct {
	Model         string `yaml:"model"`
	ResolvedModel string `yaml:"resolved-model"`
	Provider      string `yaml:"provider"`
}

type liveFailoverSettings struct {
	liveProtocolSettings `yaml:",inline"`
	SafeTestAccounts     int  `yaml:"safe-test-accounts"`
	Deterministic        bool `yaml:"deterministic-safe-fixture"`
}

type liveSettings struct {
	Claude   liveProtocolSettings `yaml:"claude"`
	Codex    liveProtocolSettings `yaml:"codex"`
	Failover liveFailoverSettings `yaml:"failover"`
}

type liveProtocolExpectation struct {
	liveProtocolSettings
	Path string
}

type liveRequestResult struct {
	Method           string
	Path             string
	RequestedModel   string
	State            string
	AccountingState  string
	DownstreamStatus int
	Attempts         []liveAttemptResult
}

type liveAttemptResult struct {
	Provider         string
	ResolvedModel    string
	RequestedAlias   string
	UpstreamAuthID   string
	UpstreamAuthType string
	Failed           bool
}

// TestLiveProtocols is intentionally gated by an operator-owned startup configuration. It emits
// only statuses and safe validation labels, never request bodies, credentials, or auth IDs.
func TestLiveProtocols(t *testing.T) {
	path := os.Getenv("LLMGW_LIVE_CONFIG")
	if path == "" {
		t.Skip("LLMGW_LIVE_CONFIG is not set")
	}
	cfg, live := loadLiveConfig(t, path)
	if live.Claude.Model == "" && live.Codex.Model == "" {
		t.Fatal("llmgw.live must configure claude or codex")
	}
	dsn, err := cfg.DatabaseDSN(os.Getenv)
	if err != nil {
		t.Fatal("live PostgreSQL DSN is unavailable")
	}
	pepper, err := cfg.KeyPepper(os.Getenv)
	if err != nil {
		t.Fatal("live key pepper is unavailable")
	}
	store, err := postgres.New(context.Background(), dsn)
	if err != nil {
		t.Fatal("open live test store failed")
	}
	queryPool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		store.Close()
		t.Fatal("open live assertion pool failed")
	}
	t.Cleanup(queryPool.Close)
	keys, err := projectkey.NewService(store, pepper, rand.Reader, time.Now)
	clear(pepper)
	if err != nil {
		store.Close()
		t.Fatal("construct live test key service failed")
	}
	created, err := keys.Create(
		context.Background(),
		"live-smoke-"+strconv.FormatInt(time.Now().UnixNano(), 36),
		"temporary-live-smoke",
		nil,
	)
	if err != nil {
		store.Close()
		t.Fatal("create temporary live project key failed")
	}
	t.Cleanup(func() {
		_ = store.RevokeKey(context.Background(), created.Key.ID, time.Now().UTC())
		store.Close()
	})

	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() {
		runDone <- command.Run(runCtx, []string{"--config", path, "serve"}, command.Streams{
			In: strings.NewReader(""), Out: io.Discard, Err: io.Discard, Getenv: os.Getenv,
		})
	}()
	baseURL := liveBaseURL(cfg)
	waitReady(t, baseURL, runDone)

	if live.Failover.Model != "" {
		if !live.Failover.Deterministic || live.Failover.SafeTestAccounts < 2 {
			t.Log("live failover smoke skipped: no deterministic fixture with two safe test accounts")
		} else {
			assertLiveFailover(
				t, queryPool, created.Key.ProjectID, baseURL, created.Plaintext, live.Failover,
			)
		}
	}
	if live.Claude.Model != "" {
		runProtocolPair(t, queryPool, created.Key.ProjectID, baseURL, created.Plaintext,
			"/v1/messages", live.Claude, claudeBody)
	}
	if live.Codex.Model != "" {
		runProtocolPair(t, queryPool, created.Key.ProjectID, baseURL, created.Plaintext,
			"/v1/responses", live.Codex, codexBody)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal("live service shutdown failed")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("live service did not stop within 30 seconds")
	}
}

func loadLiveConfig(t *testing.T, path string) (config.Config, liveSettings) {
	t.Helper()
	cfg, err := config.Load(path, os.Getenv)
	if err != nil {
		t.Fatal("load live configuration failed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read live configuration failed")
	}
	settings, err := parseLiveSettings(data)
	if err != nil {
		t.Fatal("parse live test settings failed")
	}
	return cfg, settings
}

func parseLiveSettings(data []byte) (liveSettings, error) {
	var projection liveProjection
	if err := yaml.Unmarshal(data, &projection); err != nil {
		return liveSettings{}, err
	}
	settings := projection.LLMGW.Live
	normalizeLiveProtocol(&settings.Claude)
	normalizeLiveProtocol(&settings.Codex)
	normalizeLiveProtocol(&settings.Failover.liveProtocolSettings)
	return settings, nil
}

func normalizeLiveProtocol(protocol *liveProtocolSettings) {
	protocol.Model = strings.TrimSpace(protocol.Model)
	protocol.ResolvedModel = strings.TrimSpace(protocol.ResolvedModel)
	protocol.Provider = strings.TrimSpace(protocol.Provider)
}

func liveBaseURL(cfg config.Config) string {
	host := cfg.Proxy.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + strconv.Itoa(cfg.Proxy.Port)
}

func waitReady(t *testing.T, baseURL string, runDone <-chan error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-runDone:
			t.Fatal("live service returned before readiness")
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("live service readiness timed out")
}

func runProtocolPair(
	t *testing.T,
	queryPool *pgxpool.Pool,
	projectID int64,
	baseURL string,
	key string,
	path string,
	protocol liveProtocolSettings,
	body func(string, bool) []byte,
) {
	t.Helper()
	for _, stream := range []bool{false, true} {
		beforeRequests := liveRequestCount(t, queryPool, projectID)
		status := liveRequest(t, baseURL+path, key, body(protocol.Model, stream))
		if status != http.StatusOK {
			t.Fatalf("%s stream=%t returned status %d", path, stream, status)
		}
		result := awaitLiveResult(t, queryPool, projectID, beforeRequests, 1)
		if err := validateLiveProtocolResult(result, liveProtocolExpectation{
			Path: path, liveProtocolSettings: protocol,
		}); err != nil {
			t.Fatalf("%s stream=%t normalized rows invalid: %v", path, stream, err)
		}
	}
}

func liveRequest(t *testing.T, endpoint string, key string, body []byte) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal("create live request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("send live request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func liveRequestCount(t *testing.T, queryPool *pgxpool.Pool, projectID int64) int {
	t.Helper()
	const query = `
SELECT count(*)
FROM request_event r
WHERE r.project_id = $1
  AND r.operation = 'generation'
  AND r.state = 'completed'`
	var requests int
	if err := queryPool.QueryRow(context.Background(), query, projectID).Scan(&requests); err != nil {
		t.Fatal("count live request rows failed")
	}
	return requests
}

func awaitLiveResult(
	t *testing.T,
	queryPool *pgxpool.Pool,
	projectID int64,
	offset int,
	minimumAttempts int,
) liveRequestResult {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		result, found, err := loadLiveResult(
			context.Background(), queryPool, projectID, offset,
		)
		if err != nil {
			t.Fatal("read live normalized rows failed")
		}
		if found && len(result.Attempts) >= minimumAttempts {
			return result
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("live normalized request/attempt rows did not settle")
	return liveRequestResult{}
}

func assertLiveFailover(
	t *testing.T,
	queryPool *pgxpool.Pool,
	projectID int64,
	baseURL string,
	key string,
	fixture liveFailoverSettings,
) {
	t.Helper()
	beforeRequests := liveRequestCount(t, queryPool, projectID)
	status := liveRequest(
		t, baseURL+"/v1/responses", key, codexBody(fixture.Model, false),
	)
	if status != http.StatusOK {
		t.Fatalf("declared-safe failover returned status %d", status)
	}
	result := awaitLiveResult(t, queryPool, projectID, beforeRequests, 2)
	if err := validateLiveFailoverResult(result, liveProtocolExpectation{
		Path: "/v1/responses", liveProtocolSettings: fixture.liveProtocolSettings,
	}); err != nil {
		t.Fatalf("declared-safe failover normalized rows invalid: %v", err)
	}
}

func loadLiveResult(
	ctx context.Context,
	queryPool *pgxpool.Pool,
	projectID int64,
	offset int,
) (liveRequestResult, bool, error) {
	const requestQuery = `
SELECT id, method, path, COALESCE(requested_model, ''), state,
       accounting_state, downstream_status
FROM request_event
WHERE project_id = $1
  AND operation = 'generation'
  AND state = 'completed'
ORDER BY requested_at, id
OFFSET $2 LIMIT 1`
	var requestID string
	var result liveRequestResult
	err := queryPool.QueryRow(ctx, requestQuery, projectID, offset).Scan(
		&requestID,
		&result.Method,
		&result.Path,
		&result.RequestedModel,
		&result.State,
		&result.AccountingState,
		&result.DownstreamStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return liveRequestResult{}, false, nil
	}
	if err != nil {
		return liveRequestResult{}, false, err
	}

	const attemptQuery = `
SELECT provider, resolved_model, requested_alias, upstream_auth_id,
       upstream_auth_type, failed
FROM usage_attempt
WHERE request_id = $1
ORDER BY created_at, id`
	rows, err := queryPool.Query(ctx, attemptQuery, requestID)
	if err != nil {
		return liveRequestResult{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var attempt liveAttemptResult
		if err := rows.Scan(
			&attempt.Provider,
			&attempt.ResolvedModel,
			&attempt.RequestedAlias,
			&attempt.UpstreamAuthID,
			&attempt.UpstreamAuthType,
			&attempt.Failed,
		); err != nil {
			return liveRequestResult{}, false, err
		}
		result.Attempts = append(result.Attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return liveRequestResult{}, false, err
	}
	return result, true, nil
}

func validateLiveProtocolResult(
	result liveRequestResult,
	expectation liveProtocolExpectation,
) error {
	if result.Method != http.MethodPost {
		return errors.New("normalized request method mismatch")
	}
	if result.Path != expectation.Path {
		return errors.New("normalized request path mismatch")
	}
	if result.RequestedModel != expectation.Model {
		return errors.New("normalized requested model mismatch")
	}
	if result.State != "completed" || result.AccountingState != "observed" {
		return errors.New("normalized request lifecycle mismatch")
	}
	if result.DownstreamStatus != http.StatusOK {
		return errors.New("normalized downstream status mismatch")
	}
	if len(result.Attempts) == 0 {
		return errors.New("normalized usage attempt missing")
	}
	for _, attempt := range result.Attempts {
		if attempt.Provider != expectation.Provider {
			return errors.New("normalized attempt provider mismatch")
		}
		if attempt.ResolvedModel != expectation.ResolvedModel {
			return errors.New("normalized resolved model mismatch")
		}
		if attempt.RequestedAlias != expectation.Model {
			return errors.New("normalized requested alias mismatch")
		}
		if attempt.UpstreamAuthID == "" || attempt.UpstreamAuthType == "" {
			return errors.New("normalized upstream auth metadata missing")
		}
	}
	if result.Attempts[len(result.Attempts)-1].Failed {
		return errors.New("normalized terminal attempt failed")
	}
	return nil
}

func validateLiveFailoverResult(
	result liveRequestResult,
	expectation liveProtocolExpectation,
) error {
	if err := validateLiveProtocolResult(result, expectation); err != nil {
		return err
	}
	if len(result.Attempts) != 2 {
		return errors.New("failover attempt sequence must contain exactly two attempts")
	}
	if !result.Attempts[0].Failed || result.Attempts[1].Failed {
		return errors.New("failover attempt sequence is not failed then successful")
	}
	if result.Attempts[0].UpstreamAuthID == result.Attempts[1].UpstreamAuthID {
		return errors.New("failover attempts used the same upstream auth identity")
	}
	return nil
}

func claudeBody(model string, stream bool) []byte {
	value, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 8, "stream": stream,
		"messages": []map[string]string{{"role": "user", "content": "live smoke"}},
	})
	return value
}

func codexBody(model string, stream bool) []byte {
	value, _ := json.Marshal(map[string]any{
		"model": model, "stream": stream,
		"input": "live smoke",
	})
	return value
}
