package mavensync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchAssets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/service/rest/v1/search/assets" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		q := r.URL.Query()
		if q.Get("repository") != "maven-releases" {
			t.Errorf("unexpected repository: %s", q.Get("repository"))
		}
		if q.Get("maven.groupId") != "com.test" {
			t.Errorf("unexpected groupId: %s", q.Get("maven.groupId"))
		}

		resp := SearchResponse{
			Items: []AssetItem{
				{DownloadURL: "http://example.com/artifact-1.0.jar", Path: "com/test/artifact/1.0/artifact-1.0.jar"},
				{DownloadURL: "http://example.com/artifact-1.0.pom", Path: "com/test/artifact/1.0/artifact-1.0.pom"},
			},
			ContinuationToken: "",
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	items, err := SearchAssets(srv.URL, "user", "pass", "maven-releases", "com.test", "artifact", "1.0", http.DefaultClient)
	if err != nil {
		t.Fatalf("SearchAssets failed: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	if items[0].Path != "com/test/artifact/1.0/artifact-1.0.jar" {
		t.Errorf("unexpected first item path: %s", items[0].Path)
	}
}

func TestSearchAssetsPagination(t *testing.T) {
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)

		var resp SearchResponse

		if n == 1 {
			resp = SearchResponse{
				Items: []AssetItem{
					{DownloadURL: "http://example.com/artifact-1.0.jar", Path: "artifact-1.0.jar"},
					{DownloadURL: "http://example.com/artifact-1.0.pom", Path: "artifact-1.0.pom"},
				},
				ContinuationToken: "page2token",
			}
		} else {
			// Verify continuation token is passed.
			if ct := r.URL.Query().Get("continuationToken"); ct != "page2token" {
				t.Errorf("expected continuationToken=page2token, got %q", ct)
			}

			resp = SearchResponse{
				Items: []AssetItem{
					{DownloadURL: "http://example.com/artifact-1.0-sources.jar", Path: "artifact-1.0-sources.jar"},
				},
				ContinuationToken: "",
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	items, err := SearchAssets(srv.URL, "", "", "maven-releases", "com.test", "artifact", "1.0", &http.Client{})
	if err != nil {
		t.Fatalf("SearchAssets failed: %v", err)
	}

	if len(items) != 3 {
		t.Fatalf("expected 3 items (2 + 1 from pagination), got %d", len(items))
	}

	if callCount.Load() != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", callCount.Load())
	}
}

func TestFetchComponentAssets(t *testing.T) {
	jarContent := []byte("fake-jar-bytes-here")
	pomContent := []byte("<project>fake-pom</project>")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/service/rest/v1/search/assets":
			resp := SearchResponse{
				Items: []AssetItem{
					{
						DownloadURL: "PLACEHOLDER/repository/maven-releases/com/test/artifact/1.0/artifact-1.0.jar",
						Path:        "com/test/artifact/1.0/artifact-1.0.jar",
					},
					{
						DownloadURL: "PLACEHOLDER/repository/maven-releases/com/test/artifact/1.0/artifact-1.0.pom",
						Path:        "com/test/artifact/1.0/artifact-1.0.pom",
					},
				},
				ContinuationToken: "",
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case "/repository/maven-releases/com/test/artifact/1.0/artifact-1.0.jar":
			w.Write(jarContent)

		case "/repository/maven-releases/com/test/artifact/1.0/artifact-1.0.pom":
			w.Write(pomContent)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// We need the downloadUrls to point at the test server.
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/service/rest/v1/search/assets":
			resp := SearchResponse{
				Items: []AssetItem{
					{
						DownloadURL: srv.URL + "/repository/maven-releases/com/test/artifact/1.0/artifact-1.0.jar",
						Path:        "com/test/artifact/1.0/artifact-1.0.jar",
					},
					{
						DownloadURL: srv.URL + "/repository/maven-releases/com/test/artifact/1.0/artifact-1.0.pom",
						Path:        "com/test/artifact/1.0/artifact-1.0.pom",
					},
				},
				ContinuationToken: "",
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		default:
			// Proxy to the main server for file downloads.
			srv.Config.Handler.ServeHTTP(w, r)
		}
	}))
	defer wrapper.Close()

	tempDir, files, err := FetchComponentAssets(
		wrapper.URL, "user", "pass",
		"maven-releases", "com.test", "artifact", "1.0",
		&http.Client{}, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("FetchComponentAssets failed: %v", err)
	}

	defer os.RemoveAll(tempDir)

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// Verify files exist in the correct Maven layout.
	expectedJar := filepath.Join(tempDir, "com", "test", "artifact", "1.0", "artifact-1.0.jar")
	expectedPom := filepath.Join(tempDir, "com", "test", "artifact", "1.0", "artifact-1.0.pom")

	gotJar, err := os.ReadFile(expectedJar)
	if err != nil {
		t.Fatalf("jar file not found at expected path: %v", err)
	}

	if string(gotJar) != string(jarContent) {
		t.Errorf("jar content mismatch: got %q, want %q", gotJar, jarContent)
	}

	gotPom, err := os.ReadFile(expectedPom)
	if err != nil {
		t.Fatalf("pom file not found at expected path: %v", err)
	}

	if string(gotPom) != string(pomContent) {
		t.Errorf("pom content mismatch: got %q, want %q", gotPom, pomContent)
	}
}
