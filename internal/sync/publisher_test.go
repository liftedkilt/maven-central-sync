package mavensync

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateBundle(t *testing.T) {
	// Create a temp dir with Maven layout files.
	dir := t.TempDir()

	jarPath := filepath.Join(dir, "com", "test", "artifact", "1.0", "artifact-1.0.jar")
	pomPath := filepath.Join(dir, "com", "test", "artifact", "1.0", "artifact-1.0.pom")

	if err := os.MkdirAll(filepath.Dir(jarPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(jarPath, []byte("fake-jar-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(pomPath, []byte("<project/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	bundlePath, err := CreateBundle(dir)
	if err != nil {
		t.Fatalf("CreateBundle failed: %v", err)
	}

	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("bundle file does not exist: %v", err)
	}

	// Open the ZIP and verify entries.
	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		t.Fatalf("opening bundle zip: %v", err)
	}
	defer zr.Close()

	entries := make(map[string]bool)
	for _, f := range zr.File {
		entries[f.Name] = true
	}

	expectedEntries := []string{
		"com/test/artifact/1.0/artifact-1.0.jar",
		"com/test/artifact/1.0/artifact-1.0.pom",
	}

	for _, e := range expectedEntries {
		if !entries[e] {
			t.Errorf("expected zip entry %q not found; got entries: %v", e, entries)
		}
	}

	if len(zr.File) != len(expectedEntries) {
		t.Errorf("expected %d entries, got %d", len(expectedEntries), len(zr.File))
	}
}

func TestUploadBundle(t *testing.T) {
	fakeDeploymentID := "deploy-abc-123"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		if r.URL.Path != "/api/v1/publisher/upload" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		// Verify it is multipart.
		ct := r.Header.Get("Content-Type")
		if ct == "" {
			t.Error("missing Content-Type header")
		}

		// Verify authorization header.
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("unexpected Authorization: %s", auth)
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, fakeDeploymentID)
	}))
	defer srv.Close()

	// Create a small zip file to upload.
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundle.zip")

	if err := os.WriteFile(bundlePath, []byte("PK-fake-zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := UploadBundle(bundlePath, srv.URL, "test-token", "AUTOMATIC", &http.Client{})
	if err != nil {
		t.Fatalf("UploadBundle failed: %v", err)
	}

	if id != fakeDeploymentID {
		t.Errorf("expected deployment ID %q, got %q", fakeDeploymentID, id)
	}
}

func TestWaitForPublication(t *testing.T) {
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/publisher/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		n := callCount.Add(1)

		var state string
		switch n {
		case 1:
			state = "PENDING"
		case 2:
			state = "VALIDATING"
		default:
			state = "PUBLISHED"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"deploymentId":    "deploy-123",
			"deploymentState": state,
		})
	}))
	defer srv.Close()

	status, err := WaitForPublication("deploy-123", srv.URL, "test-token", &http.Client{}, 30*time.Second, 1*time.Second)
	if err != nil {
		t.Fatalf("WaitForPublication failed: %v", err)
	}

	finalState, _ := status["deploymentState"].(string)
	if finalState != "PUBLISHED" {
		t.Errorf("expected PUBLISHED, got %s", finalState)
	}

	if callCount.Load() < 3 {
		t.Errorf("expected at least 3 calls, got %d", callCount.Load())
	}
}

func TestPublish(t *testing.T) {
	var uploadCalled atomic.Bool
	var statusCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/publisher/upload" && r.Method == http.MethodPost:
			uploadCalled.Store(true)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, "deploy-e2e-456")

		case r.URL.Path == "/api/v1/publisher/status" && r.Method == http.MethodPost:
			n := statusCalls.Add(1)
			state := "PENDING"
			if n >= 2 {
				state = "PUBLISHED"
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"deploymentId":    "deploy-e2e-456",
				"deploymentState": state,
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Create temp dir with test files.
	dir := t.TempDir()
	artifactDir := filepath.Join(dir, "com", "test", "artifact", "1.0")

	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(artifactDir, "artifact-1.0.jar"), []byte("jar-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(artifactDir, "artifact-1.0.pom"), []byte("<pom/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := Publish(dir, srv.URL, "test-token", &http.Client{}, 30*time.Second, 1*time.Second)
	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}

	if !uploadCalled.Load() {
		t.Error("upload endpoint was never called")
	}

	finalState, _ := status["deploymentState"].(string)
	if finalState != "PUBLISHED" {
		t.Errorf("expected PUBLISHED, got %s", finalState)
	}
}
