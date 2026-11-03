package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const maxWorkerResponse = 1 << 20

// Client talks HTTP over the worker's Unix socket. Keep-alives are disabled so
// a killed worker never leaves a stale pooled connection behind.
type Client struct {
	http *http.Client
}

// NewClient creates a Unix-socket HTTP client for socketPath.
func NewClient(socketPath string) *Client {
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{http: &http.Client{Transport: tr}}
}

// Ready returns true only for HTTP 200 from /ready. HTTP 503 means the worker
// is alive but still loading/quantizing the model. Other errors mean the worker
// is unusable.
func (c *Client) Ready(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/ready", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusServiceUnavailable:
		return false, nil
	default:
		return false, fmt.Errorf("worker /ready returned HTTP %d", resp.StatusCode)
	}
}

// Translate sends one request to the worker. The context is controlled by the
// gateway's translation timeout. Error values intentionally contain no input
// or output text.
func (c *Client) Translate(ctx context.Context, text string) (string, error) {
	body, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/translate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("worker /translate returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxWorkerResponse+1))
	if err != nil {
		return "", fmt.Errorf("read worker response: %w", err)
	}
	if len(data) > maxWorkerResponse {
		return "", errors.New("worker response too large")
	}
	var out struct {
		Translation string `json:"translation"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode worker response: %w", err)
	}
	return out.Translation, nil
}

// ReadyPollInterval is the cadence used while a worker is loading.
const ReadyPollInterval = 200 * time.Millisecond
