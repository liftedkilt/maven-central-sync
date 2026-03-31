package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mavensync "github.com/liftedkilt/maven-central-sync/internal/sync"
)

// newTestServers creates mock Nexus and Central httptest servers for full-flow tests.
func newTestServers(t *testing.T) (nexusServer *httptest.Server, centralServer *httptest.Server) {
	t.Helper()

	jarContent := []byte("test-jar-content-bytes")
	pomContent := []byte("<project>test-pom</project>")

	// Base Nexus server for file downloads.
	nexusDownload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repository/maven-releases/com/example/portal/1.0.0/portal-1.0.0.jar":
			w.Write(jarContent)
		case "/repository/maven-releases/com/example/portal/1.0.0/portal-1.0.0.pom":
			w.Write(pomContent)
		default:
			t.Errorf("nexus download: unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(nexusDownload.Close)

	// Nexus wrapper that serves search results with correct download URLs.
	nexusServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/service/rest/v1/search/assets":
			resp := mavensync.SearchResponse{
				Items: []mavensync.AssetItem{
					{
						DownloadURL: nexusDownload.URL + "/repository/maven-releases/com/example/portal/1.0.0/portal-1.0.0.jar",
						Path:        "com/example/portal/1.0.0/portal-1.0.0.jar",
					},
					{
						DownloadURL: nexusDownload.URL + "/repository/maven-releases/com/example/portal/1.0.0/portal-1.0.0.pom",
						Path:        "com/example/portal/1.0.0/portal-1.0.0.pom",
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		default:
			nexusDownload.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(nexusServer.Close)

	// Mock Maven Central server.
	var statusCalls atomic.Int32

	centralServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/publisher/upload" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, "deploy-full-flow-789")
		case r.URL.Path == "/api/v1/publisher/status" && r.Method == http.MethodPost:
			n := statusCalls.Add(1)
			state := "PENDING"
			if n >= 2 {
				state = "PUBLISHED"
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"deploymentId":    "deploy-full-flow-789",
				"deploymentState": state,
			})
		default:
			t.Errorf("central: unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(centralServer.Close)

	return nexusServer, centralServer
}

func newTestConfig(nexusURL, centralURL string) Config {
	return Config{
		NexusURL:            nexusURL,
		NexusUsername:        "testuser",
		NexusPassword:       "testpass",
		MavenCentralURL:     centralURL,
		MavenCentralToken:   "test-token",
		WorkerConcurrency:   1,
		FetchRetryTimeout:   5 * time.Second,
		HTTPTimeout:         10 * time.Second,
		PublishTimeout:      30 * time.Second,
		PublishPollInterval: 1 * time.Second,
	}
}

const validPayload = `{
	"action": "CREATED",
	"repositoryName": "maven-releases",
	"component": {
		"format": "maven2",
		"group": "com.example",
		"name": "portal",
		"version": "1.0.0"
	}
}`

func waitForStatus(t *testing.T, worker *Worker, gav string, timeout time.Duration) *SyncStatus {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		status := worker.GetStatus(gav)
		if status != nil && (status.State == "completed" || status.State == "failed") {
			return status
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for job %s to finish", gav)

	return nil
}

func TestWebhookAccepted(t *testing.T) {
	nexusServer, centralServer := newTestServers(t)
	cfg := newTestConfig(nexusServer.URL, centralServer.URL)
	worker := NewWorker(cfg)
	worker.Start()

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(validPayload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler := webhookHandler(cfg, worker)
	handler(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp["status"] != "accepted" {
		t.Errorf("expected status=accepted, got %v", resp["status"])
	}

	// Wait for the worker to process the job.
	gav := "com.example:portal:1.0.0"
	status := waitForStatus(t, worker, gav, 15*time.Second)

	if status.State != "completed" {
		t.Errorf("expected state=completed, got %s (error: %s)", status.State, status.Error)
	}
}

func TestWebhookIgnoresNonCreated(t *testing.T) {
	cfg := Config{}
	worker := NewWorker(cfg)
	worker.Start()

	payload := `{
		"action": "UPDATED",
		"repositoryName": "maven-releases",
		"component": {
			"format": "maven2",
			"group": "com.example",
			"name": "portal",
			"version": "1.0.0"
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	webhookHandler(cfg, worker)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp["status"] != "ignored" {
		t.Errorf("expected status=ignored, got %v", resp["status"])
	}
}

func TestWebhookIgnoresNonMaven(t *testing.T) {
	cfg := Config{}
	worker := NewWorker(cfg)
	worker.Start()

	payload := `{
		"action": "CREATED",
		"repositoryName": "npm-hosted",
		"component": {
			"format": "npm",
			"group": "my-scope",
			"name": "my-package",
			"version": "2.0.0"
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	webhookHandler(cfg, worker)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp["status"] != "ignored" {
		t.Errorf("expected status=ignored, got %v", resp["status"])
	}
}

func TestWebhookInvalidJSON(t *testing.T) {
	cfg := Config{}
	worker := NewWorker(cfg)
	worker.Start()

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("not json at all{{{"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	webhookHandler(cfg, worker)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp["error"] != "invalid JSON" {
		t.Errorf("expected error='invalid JSON', got %v", resp["error"])
	}
}

func TestWebhookHMACValid(t *testing.T) {
	nexusServer, centralServer := newTestServers(t)
	cfg := newTestConfig(nexusServer.URL, centralServer.URL)
	cfg.WebhookSecret = "test-secret"
	worker := NewWorker(cfg)
	worker.Start()

	body := []byte(validPayload)

	// Compute HMAC-SHA1 signature.
	mac := hmac.New(sha1.New, []byte("test-secret"))
	mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nexus-Webhook-Signature", signature)
	rec := httptest.NewRecorder()

	webhookHandler(cfg, worker)(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp["status"] != "accepted" {
		t.Errorf("expected status=accepted, got %v", resp["status"])
	}
}

func TestWebhookHMACInvalid(t *testing.T) {
	cfg := Config{WebhookSecret: "test-secret"}
	worker := NewWorker(cfg)
	worker.Start()

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(validPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nexus-Webhook-Signature", "deadbeef")
	rec := httptest.NewRecorder()

	webhookHandler(cfg, worker)(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp["error"] != "invalid signature" {
		t.Errorf("expected error='invalid signature', got %v", resp["error"])
	}
}

func TestWebhookHMACSkippedWhenNoSecret(t *testing.T) {
	nexusServer, centralServer := newTestServers(t)
	cfg := newTestConfig(nexusServer.URL, centralServer.URL)
	cfg.WebhookSecret = ""
	worker := NewWorker(cfg)
	worker.Start()

	// No signature header sent.
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(validPayload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	webhookHandler(cfg, worker)(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp["status"] != "accepted" {
		t.Errorf("expected status=accepted, got %v", resp["status"])
	}
}

func TestWebhookDeduplication(t *testing.T) {
	nexusServer, centralServer := newTestServers(t)
	cfg := newTestConfig(nexusServer.URL, centralServer.URL)
	worker := NewWorker(cfg)
	worker.Start()

	// First request should be accepted.
	req1 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(validPayload))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()

	webhookHandler(cfg, worker)(rec1, req1)

	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first request: expected 202, got %d; body: %s", rec1.Code, rec1.Body.String())
	}

	var resp1 map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("decoding first response: %v", err)
	}

	if resp1["status"] != "accepted" {
		t.Errorf("first request: expected status=accepted, got %v", resp1["status"])
	}

	// Second request with same payload should be duplicate.
	req2 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(validPayload))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()

	webhookHandler(cfg, worker)(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("second request: expected 200, got %d; body: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decoding second response: %v", err)
	}

	if resp2["status"] != "duplicate" {
		t.Errorf("second request: expected status=duplicate, got %v", resp2["status"])
	}
}
