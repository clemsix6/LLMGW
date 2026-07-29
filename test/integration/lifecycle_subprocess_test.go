package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestGracefulShutdownSubprocess catches a process-root mutation that closes PostgreSQL before an
// active SDK request publishes and durably drains its final usage callback.
func TestGracefulShutdownSubprocess(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "llmgw")
	build := exec.Command("go", "build", "-o", binary, "./cmd/llmgw")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build subprocess binary: %v\n%s", err, output)
	}

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, testHarness.configYAML(port), 0o600); err != nil {
		t.Fatalf("write subprocess config: %v", err)
	}
	process := exec.Command(binary, "--config", configPath)
	process.Env = append(os.Environ(),
		"TEST_POSTGRES_DSN="+testHarness.db.Config().ConnString(),
		"TEST_KEY_PEPPER=integration-key-pepper-32-bytes!!",
	)
	var processOutput bytes.Buffer
	process.Stdout = &processOutput
	process.Stderr = &processOutput
	if err := process.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	t.Cleanup(func() {
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_, _ = process.Process.Wait()
		}
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForSubprocessReady(t, baseURL, process)
	created := testHarness.createKey(t, "subprocess-drain")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	response := streamingUsageResponse(7, 3)
	response.Started = started
	response.Release = release
	testHarness.Upstream.Enqueue(response)

	requestDone := make(chan error, 1)
	go func() {
		requestDone <- sendSubprocessStream(baseURL, created.Plaintext)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess upstream request did not start")
	}
	if err := process.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal subprocess: %v", err)
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("streaming subprocess request: %v", err)
	}

	exitDone := make(chan error, 1)
	go func() { exitDone <- process.Wait() }()
	select {
	case err := <-exitDone:
		if err != nil {
			t.Fatalf("subprocess exit: %v\n%s", err, processOutput.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("subprocess did not exit within 30 seconds")
	}

	var requests, completed, attempts int
	const query = `
SELECT count(*),
       count(*) FILTER (WHERE r.state = 'completed'),
       count(a.id)
FROM request_event r
LEFT JOIN usage_attempt a ON a.request_id = r.id
WHERE r.project_id = $1`
	if err := testHarness.db.QueryRow(
		context.Background(), query, created.Key.ProjectID,
	).Scan(&requests, &completed, &attempts); err != nil {
		t.Fatalf("query drained subprocess rows: %v", err)
	}
	if requests != 1 || completed != 1 || attempts != 1 {
		t.Fatalf(
			"drained subprocess rows = requests %d completed %d attempts %d, want 1/1/1",
			requests, completed, attempts,
		)
	}
}

func waitForSubprocessReady(t *testing.T, baseURL string, process *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if process.ProcessState != nil {
			t.Fatal("subprocess exited before readiness")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("subprocess did not become ready")
}

func sendSubprocessStream(baseURL string, key string) error {
	fixture := `{
		"model":"test-model",
		"messages":[{"role":"user","content":"fixture-prompt"}],
		"stream":true,
		"stream_options":{"include_usage":true}
	}`
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		baseURL+"/v1/chat/completions",
		bytes.NewBufferString(fixture),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", response.StatusCode)
	}
	return nil
}
