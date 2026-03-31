package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func resetState() {
	mu.Lock()
	deployments = make(map[string]*Deployment)
	mu.Unlock()
}

func newTestServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/publisher/upload", handleUpload)
	mux.HandleFunc("/api/v1/publisher/status", handleStatus)
	mux.HandleFunc("/api/v1/publisher/deployment/", handleDeployment)
	return httptest.NewServer(mux)
}

// createUploadBody builds a multipart form body with a "bundle" file field.
func createUploadBody(t *testing.T) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("bundle", "test-bundle.zip")
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	// Write some fake bytes as the bundle content.
	part.Write([]byte("PK\x03\x04fake-zip-content"))
	w.Close()
	return &buf, w.FormDataContentType()
}

// doUpload performs an upload request and returns the HTTP response.
func doUpload(t *testing.T, ts *httptest.Server, auth string, publishingType string, includeBundle bool) *http.Response {
	t.Helper()

	var body io.Reader
	var contentType string

	if includeBundle {
		buf, ct := createUploadBody(t)
		body = buf
		contentType = ct
	} else {
		// Send an empty multipart form with no file field.
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.Close()
		body = &buf
		contentType = w.FormDataContentType()
	}

	url := ts.URL + "/api/v1/publisher/upload"
	if publishingType != "" {
		url += "?publishingType=" + publishingType
	}

	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("performing request: %v", err)
	}
	return resp
}

// readBody reads and returns the response body as a string.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(b)
}

// pollStatus calls the status endpoint and returns the parsed JSON map and HTTP status code.
func pollStatus(t *testing.T, ts *httptest.Server, deploymentID string) (map[string]any, int) {
	t.Helper()
	url := ts.URL + "/api/v1/publisher/status?id=" + deploymentID
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("creating status request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("performing status request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return map[string]any{"body": string(b)}, resp.StatusCode
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding status response: %v", err)
	}
	return result, resp.StatusCode
}

func TestUploadAutomatic(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	resp := doUpload(t, ts, "Bearer my-token", "AUTOMATIC", true)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatal("expected non-empty deployment ID")
	}
}

func TestUploadUserManaged(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	resp := doUpload(t, ts, "Bearer my-token", "USER_MANAGED", true)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatal("expected non-empty deployment ID")
	}
}

func TestUploadMissingAuth(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	resp := doUpload(t, ts, "", "AUTOMATIC", true)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", resp.StatusCode, body)
	}
}

func TestUploadMissingBundle(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	resp := doUpload(t, ts, "Bearer my-token", "AUTOMATIC", false)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}

func TestUploadInvalidPublishingType(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	resp := doUpload(t, ts, "Bearer my-token", "INVALID_TYPE", true)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Invalid publishingType") {
		t.Fatalf("expected error about publishingType, got: %s", body)
	}
}

func TestStatusAutomaticStateProgression(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	resp := doUpload(t, ts, "Bearer my-token", "AUTOMATIC", true)
	deploymentID := strings.TrimSpace(readBody(t, resp))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload failed: %d", resp.StatusCode)
	}

	// The status endpoint advances AUTOMATIC deployments one step per poll.
	// Initial state is PENDING. Each poll advances it.
	expectedStates := []string{"VALIDATING", "VALIDATED", "PUBLISHING", "PUBLISHED"}

	for _, expected := range expectedStates {
		result, code := pollStatus(t, ts, deploymentID)
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		state := result["deploymentState"].(string)
		if state != expected {
			t.Fatalf("expected state %s, got %s", expected, state)
		}
	}

	// One more poll should stay at PUBLISHED.
	result, _ := pollStatus(t, ts, deploymentID)
	state := result["deploymentState"].(string)
	if state != "PUBLISHED" {
		t.Fatalf("expected state to remain PUBLISHED, got %s", state)
	}
}

func TestStatusUserManagedStopsAtValidated(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	resp := doUpload(t, ts, "Bearer my-token", "USER_MANAGED", true)
	deploymentID := strings.TrimSpace(readBody(t, resp))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload failed: %d", resp.StatusCode)
	}

	// USER_MANAGED advances through PENDING->VALIDATING->VALIDATED on polls,
	// then stops at VALIDATED.
	expectedStates := []string{"VALIDATING", "VALIDATED"}

	for _, expected := range expectedStates {
		result, code := pollStatus(t, ts, deploymentID)
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		state := result["deploymentState"].(string)
		if state != expected {
			t.Fatalf("expected state %s, got %s", expected, state)
		}
	}

	// Additional polls should remain at VALIDATED.
	for i := 0; i < 3; i++ {
		result, _ := pollStatus(t, ts, deploymentID)
		state := result["deploymentState"].(string)
		if state != "VALIDATED" {
			t.Fatalf("expected state to remain VALIDATED, got %s (poll %d)", state, i)
		}
	}
}

func TestStatusMissingAuth(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	url := ts.URL + "/api/v1/publisher/status?id=some-id"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	// No Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestStatusUnknownDeployment(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	_, code := pollStatus(t, ts, "nonexistent-id")
	if code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", code)
	}
}

func TestPublishUserManaged(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	// Upload with USER_MANAGED.
	resp := doUpload(t, ts, "Bearer my-token", "USER_MANAGED", true)
	deploymentID := strings.TrimSpace(readBody(t, resp))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload failed: %d", resp.StatusCode)
	}

	// Poll until VALIDATED.
	for i := 0; i < 5; i++ {
		result, _ := pollStatus(t, ts, deploymentID)
		if result["deploymentState"].(string) == "VALIDATED" {
			break
		}
	}

	// Publish via POST /api/v1/publisher/deployment/{id}.
	pubURL := ts.URL + "/api/v1/publisher/deployment/" + deploymentID
	req, err := http.NewRequest(http.MethodPost, pubURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer my-token")

	pubResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer pubResp.Body.Close()

	if pubResp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(pubResp.Body)
		t.Fatalf("expected 204, got %d: %s", pubResp.StatusCode, string(b))
	}

	// The publish goroutine advances PUBLISHING->PUBLISHED after ~1 second.
	// Poll until PUBLISHED (with timeout).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result, _ := pollStatus(t, ts, deploymentID)
		if result["deploymentState"].(string) == "PUBLISHED" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("deployment did not reach PUBLISHED state within timeout")
}

func TestPublishWrongState(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	// Upload with AUTOMATIC (starts in PENDING).
	resp := doUpload(t, ts, "Bearer my-token", "AUTOMATIC", true)
	deploymentID := strings.TrimSpace(readBody(t, resp))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload failed: %d", resp.StatusCode)
	}

	// Immediately try to publish (state is PENDING, not VALIDATED).
	pubURL := ts.URL + "/api/v1/publisher/deployment/" + deploymentID
	req, err := http.NewRequest(http.MethodPost, pubURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer my-token")

	pubResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer pubResp.Body.Close()

	if pubResp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(pubResp.Body)
		t.Fatalf("expected 400, got %d: %s", pubResp.StatusCode, string(b))
	}
}

func TestDropValidated(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	// Upload with USER_MANAGED.
	resp := doUpload(t, ts, "Bearer my-token", "USER_MANAGED", true)
	deploymentID := strings.TrimSpace(readBody(t, resp))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload failed: %d", resp.StatusCode)
	}

	// Poll until VALIDATED.
	for i := 0; i < 5; i++ {
		result, _ := pollStatus(t, ts, deploymentID)
		if result["deploymentState"].(string) == "VALIDATED" {
			break
		}
	}

	// DROP via DELETE.
	delURL := ts.URL + "/api/v1/publisher/deployment/" + deploymentID
	req, err := http.NewRequest(http.MethodDelete, delURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer my-token")

	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer delResp.Body.Close()

	if delResp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(delResp.Body)
		t.Fatalf("expected 204, got %d: %s", delResp.StatusCode, string(b))
	}

	// Status should now return 404.
	_, code := pollStatus(t, ts, deploymentID)
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 after drop, got %d", code)
	}
}

func TestDropWrongState(t *testing.T) {
	resetState()
	ts := newTestServer()
	defer ts.Close()

	// Upload (starts in PENDING).
	resp := doUpload(t, ts, "Bearer my-token", "USER_MANAGED", true)
	deploymentID := strings.TrimSpace(readBody(t, resp))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload failed: %d", resp.StatusCode)
	}

	// Immediately try to drop (state is PENDING, not VALIDATED or FAILED).
	delURL := ts.URL + "/api/v1/publisher/deployment/" + deploymentID
	req, err := http.NewRequest(http.MethodDelete, delURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer my-token")

	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer delResp.Body.Close()

	if delResp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(delResp.Body)
		t.Fatalf("expected 400, got %d: %s", delResp.StatusCode, string(b))
	}
}
