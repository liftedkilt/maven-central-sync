package mavensync

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CreateBundle walks artifactDir and creates a bundle.zip containing all files
// with paths relative to artifactDir.
func CreateBundle(artifactDir string) (string, error) {
	bundlePath := filepath.Join(artifactDir, "bundle.zip")

	outFile, err := os.Create(bundlePath)
	if err != nil {
		return "", fmt.Errorf("creating bundle file: %w", err)
	}
	defer outFile.Close()

	zw := zip.NewWriter(outFile)
	defer zw.Close()

	var entries []string

	err = filepath.Walk(artifactDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		if info.Name() == "bundle.zip" {
			return nil
		}

		relPath, err := filepath.Rel(artifactDir, path)
		if err != nil {
			return fmt.Errorf("computing relative path: %w", err)
		}

		// Use forward slashes in the zip archive.
		arcName := filepath.ToSlash(relPath)

		w, err := zw.Create(arcName)
		if err != nil {
			return fmt.Errorf("creating zip entry %s: %w", arcName, err)
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("opening %s: %w", path, err)
		}
		defer f.Close()

		if _, err := io.Copy(w, f); err != nil {
			return fmt.Errorf("writing %s to zip: %w", arcName, err)
		}

		entries = append(entries, arcName)
		slog.Debug("Added to bundle", "entry", arcName)

		return nil
	})

	if err != nil {
		return "", fmt.Errorf("walking artifact dir: %w", err)
	}

	slog.Info("Created bundle",
		"path", bundlePath,
		"entries", len(entries),
		"files", entries,
	)

	return bundlePath, nil
}

// UploadBundle uploads a ZIP bundle to Maven Central's Publisher API and
// returns the deployment ID.
func UploadBundle(bundlePath, centralURL, token, publishingType string, client *http.Client) (string, error) {
	if publishingType == "" {
		publishingType = "AUTOMATIC"
	}

	url := centralURL + "/api/v1/publisher/upload?publishingType=" + publishingType

	// Build a multipart body using a pipe so we don't buffer in memory.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer mw.Close()

		part, err := mw.CreateFormFile("bundle", filepath.Base(bundlePath))
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		f, err := os.Open(bundlePath)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		defer f.Close()

		if _, err := io.Copy(part, f); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()

	req, err := http.NewRequest(http.MethodPost, url, pr)
	if err != nil {
		return "", fmt.Errorf("creating upload request: %w", err)
	}

	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("uploading bundle: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		slog.Error("Upload failed",
			"status", resp.StatusCode,
			"body", string(body),
		)

		return "", fmt.Errorf("upload returned status %d: %s", resp.StatusCode, string(body))
	}

	deploymentID := strings.TrimSpace(string(body))
	slog.Info("Upload successful", "deploymentId", deploymentID)

	return deploymentID, nil
}

// WaitForPublication polls the Maven Central status endpoint until the
// deployment reaches PUBLISHED or FAILED, or the timeout expires.
func WaitForPublication(deploymentID, centralURL, token string, client *http.Client, timeout time.Duration, pollInterval time.Duration) (map[string]any, error) {
	url := centralURL + "/api/v1/publisher/status?id=" + deploymentID
	deadline := time.Now().Add(timeout)
	previousState := ""

	for {
		req, err := http.NewRequest(http.MethodPost, url, nil)
		if err != nil {
			return nil, fmt.Errorf("creating status request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("polling status: %w", err)
		}

		var status map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding status response: %w", err)
		}

		resp.Body.Close()

		currentState, _ := status["deploymentState"].(string)

		if currentState != previousState {
			slog.Info("Deployment state changed",
				"deploymentId", deploymentID,
				"from", previousState,
				"to", currentState,
			)

			previousState = currentState
		}

		if currentState == "PUBLISHED" || currentState == "FAILED" {
			return status, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"deployment %s did not finish within %s; last state: %s",
				deploymentID, timeout, currentState,
			)
		}

		time.Sleep(pollInterval)
	}
}

// Publish orchestrates the full publish workflow: bundle, upload, and wait.
func Publish(artifactDir, centralURL, token string, client *http.Client, publishTimeout time.Duration, pollInterval time.Duration) (map[string]any, error) {
	slog.Info("Starting publish workflow", "artifactDir", artifactDir)

	bundlePath, err := CreateBundle(artifactDir)
	if err != nil {
		return nil, fmt.Errorf("creating bundle: %w", err)
	}

	slog.Info("Bundle created", "path", bundlePath)

	deploymentID, err := UploadBundle(bundlePath, centralURL, token, "AUTOMATIC", client)
	if err != nil {
		return nil, fmt.Errorf("uploading bundle: %w", err)
	}

	slog.Info("Uploaded bundle, waiting for publication", "deploymentId", deploymentID)

	status, err := WaitForPublication(deploymentID, centralURL, token, client, publishTimeout, pollInterval)
	if err != nil {
		return nil, fmt.Errorf("waiting for publication: %w", err)
	}

	finalState, _ := status["deploymentState"].(string)
	slog.Info("Publish workflow complete",
		"deploymentId", deploymentID,
		"state", finalState,
	)

	return status, nil
}
