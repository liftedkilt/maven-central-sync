package mavensync

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SearchResponse represents the Nexus 3 search/assets API response.
type SearchResponse struct {
	Items             []AssetItem `json:"items"`
	ContinuationToken string      `json:"continuationToken"`
}

// AssetItem represents a single asset returned by the Nexus search API.
type AssetItem struct {
	DownloadURL string `json:"downloadUrl"`
	Path        string `json:"path"`
}

// SearchAssets queries the Nexus 3 Search Assets API, handling pagination.
func SearchAssets(
	nexusURL, username, password string,
	repository, groupID, artifactID, version string,
	client *http.Client,
) ([]AssetItem, error) {
	baseURL := nexusURL + "/service/rest/v1/search/assets"
	var allAssets []AssetItem
	continuationToken := ""

	for {
		req, err := http.NewRequest(http.MethodGet, baseURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}

		q := req.URL.Query()
		q.Set("repository", repository)
		q.Set("maven.groupId", groupID)
		q.Set("maven.artifactId", artifactID)
		q.Set("maven.baseVersion", version)

		if continuationToken != "" {
			q.Set("continuationToken", continuationToken)
		}

		req.URL.RawQuery = q.Encode()

		if username != "" && password != "" {
			req.SetBasicAuth(username, password)
		}

		slog.Info("Searching assets",
			"repository", repository,
			"groupId", groupID,
			"artifactId", artifactID,
			"version", version,
			"continuationToken", continuationToken,
		)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("searching assets: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("search returned status %d: %s", resp.StatusCode, string(body))
		}

		var sr SearchResponse
		if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
			return nil, fmt.Errorf("decoding search response: %w", err)
		}

		allAssets = append(allAssets, sr.Items...)
		continuationToken = sr.ContinuationToken

		if continuationToken == "" {
			break
		}
	}

	slog.Info("Search complete",
		"count", len(allAssets),
		"groupId", groupID,
		"artifactId", artifactID,
		"version", version,
	)

	return allAssets, nil
}

// assetRelativePath builds a Maven-layout relative path:
// com/example/artifact/1.0.0/file.jar
func assetRelativePath(groupID, artifactID, version, filename string) string {
	groupPath := strings.ReplaceAll(groupID, ".", string(filepath.Separator))

	return filepath.Join(groupPath, artifactID, version, filename)
}

// FetchComponentAssets downloads all assets for a Maven component into a temp
// directory using standard Maven repository layout. It returns the temp
// directory path and a list of relative file paths.
//
// Because Nexus webhooks fire before assets are fully indexed, this function
// retries the search with exponential backoff when zero assets are found.
// After finding assets, it performs a settle check to ensure all assets are indexed.
func FetchComponentAssets(
	nexusURL, username, password string,
	repository, groupID, artifactID, version string,
	client *http.Client,
	fetchTimeout time.Duration,
) (string, []string, error) {
	var assets []AssetItem
	var err error

	deadline := time.Now().Add(fetchTimeout)
	delay := 2 * time.Second
	maxDelay := 15 * time.Second

	// Retry with exponential backoff until assets are found or timeout.
	for attempt := 0; ; attempt++ {
		assets, err = SearchAssets(nexusURL, username, password, repository, groupID, artifactID, version, client)
		if err != nil {
			return "", nil, err
		}

		if len(assets) > 0 {
			break
		}

		if time.Now().Add(delay).After(deadline) {
			slog.Warn("No assets found after retries",
				"groupId", groupID,
				"artifactId", artifactID,
				"version", version,
				"repository", repository,
				"attempts", attempt+1,
			)
			return "", nil, nil
		}

		slog.Info("No assets yet, retrying after indexing delay",
			"groupId", groupID,
			"artifactId", artifactID,
			"version", version,
			"attempt", attempt+1,
			"delay", delay,
		)
		time.Sleep(delay)

		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}

	// Settle check: wait for asset count to stabilize.
	for {
		settleDelay := 5 * time.Second
		if time.Now().Add(settleDelay).After(deadline) {
			break
		}

		slog.Info("Settle check: waiting for asset count to stabilize",
			"currentCount", len(assets),
			"delay", settleDelay,
		)
		time.Sleep(settleDelay)

		newAssets, err := SearchAssets(nexusURL, username, password, repository, groupID, artifactID, version, client)
		if err != nil {
			return "", nil, err
		}

		if len(newAssets) <= len(assets) {
			slog.Info("Asset count stable", "count", len(newAssets))
			assets = newAssets
			break
		}

		slog.Info("Asset count increased, rechecking",
			"previous", len(assets),
			"current", len(newAssets),
		)
		assets = newAssets
	}

	tempDir, err := os.MkdirTemp("", "maven-sync-")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir: %w", err)
	}

	var downloaded []string

	for _, asset := range assets {
		if asset.DownloadURL == "" {
			slog.Warn("Asset missing downloadUrl, skipping", "asset", asset)

			continue
		}

		// Extract filename from the download URL.
		parts := strings.Split(asset.DownloadURL, "/")
		filename := parts[len(parts)-1]

		relPath := assetRelativePath(groupID, artifactID, version, filename)
		dest := filepath.Join(tempDir, relPath)

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return tempDir, downloaded, fmt.Errorf("creating directory for %s: %w", relPath, err)
		}

		slog.Info("Downloading", "url", asset.DownloadURL, "dest", dest)

		if err := downloadFile(dest, asset.DownloadURL, username, password, client); err != nil {
			return tempDir, downloaded, fmt.Errorf("downloading %s: %w", asset.DownloadURL, err)
		}

		info, _ := os.Stat(dest)
		slog.Info("Saved", "path", relPath, "bytes", info.Size())

		downloaded = append(downloaded, relPath)
	}

	slog.Info("Download complete",
		"count", len(downloaded),
		"tempDir", tempDir,
		"groupId", groupID,
		"artifactId", artifactID,
		"version", version,
	)

	return tempDir, downloaded, nil
}

// downloadFile streams a URL to a local file using io.Copy.
func downloadFile(dest, url, username, password string, client *http.Client) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	if username != "" && password != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download returned status %d: %s", resp.StatusCode, string(body))
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)

	return err
}
