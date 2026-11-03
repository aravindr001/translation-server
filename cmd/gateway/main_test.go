package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"hindimalayalamtranslator/internal/auth"
	"hindimalayalamtranslator/internal/logutil"
)

// TestGatewayHelperProcess is not a real test. It lets the test binary act as
// the gateway child process so the real signal handling path can be exercised.
func TestGatewayHelperProcess(t *testing.T) {
	if os.Getenv("TRANSLATOR_TEST_HELPER") != "1" {
		t.Skip("helper process only")
	}
	if err := run(logutil.New()); err != nil {
		fmt.Fprintln(os.Stderr, "helper gateway failed:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeKeys(t *testing.T, path string, entries ...auth.KeyEntry) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"keys": entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func subprocessEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides)+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "TRANSLATOR_") || strings.HasPrefix(entry, "NOTIFY_SOCKET=") ||
			strings.HasPrefix(entry, "WATCHDOG_USEC=") {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func statusCode(client *http.Client, base, key string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/status", nil)
	if err != nil {
		return 0, "", err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(body), nil
}

func TestGatewayReloadsKeysOnSIGHUP(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "keys.json")
	socket := filepath.Join(dir, "worker.sock")
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	oldRaw, oldEntry, err := auth.GenerateKey("old")
	if err != nil {
		t.Fatal(err)
	}
	writeKeys(t, keyFile, oldEntry)

	cmd := exec.Command(os.Args[0], "-test.run=^TestGatewayHelperProcess$")
	cmd.Env = subprocessEnv(map[string]string{
		"TRANSLATOR_TEST_HELPER":      "1",
		"TRANSLATOR_PORT":             fmt.Sprint(port),
		"TRANSLATOR_SOCKET_PATH":      socket,
		"TRANSLATOR_KEY_FILE":         keyFile,
		"TRANSLATOR_WORKER_PYTHON":    "/bin/true",
		"TRANSLATOR_WORKER_SCRIPT":    "/bin/true",
		"TRANSLATOR_IDLE_TIMEOUT":     "1m",
		"TRANSLATOR_STARTUP_COOLDOWN": "0s",
	})
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	healthy := false
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("gateway did not become healthy; output:\n%s", output.String())
	}

	code, body, err := statusCode(client, base, oldRaw)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK {
		t.Fatalf("old key before rotation = %d (body %s)", code, body)
	}

	newRaw, newEntry, err := auth.GenerateKey("new")
	if err != nil {
		t.Fatal(err)
	}
	writeKeys(t, keyFile, newEntry)
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(5 * time.Second)
	reloaded := false
	for time.Now().Before(deadline) {
		code, _, err := statusCode(client, base, newRaw)
		if err == nil && code == http.StatusOK {
			reloaded = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !reloaded {
		t.Fatalf("new key was not accepted after SIGHUP; output:\n%s", output.String())
	}
	code, body, err = statusCode(client, base, oldRaw)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("old key after rotation = %d, want 401 (body %s)", code, body)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		if err != nil {
			t.Fatalf("gateway exited with error: %v; output:\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("gateway did not exit after SIGTERM; output:\n%s", output.String())
	}
}

func TestGenkeyHashMatchesRawKey(t *testing.T) {
	raw, entry, err := auth.GenerateKey("sample")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(raw))
	if entry.SHA256Hash != hex.EncodeToString(sum[:]) {
		t.Fatal("generated hash does not match raw key")
	}
}
