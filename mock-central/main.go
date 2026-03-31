package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crypto/rand"
)

// State machine: PENDING -> VALIDATING -> VALIDATED -> PUBLISHING -> PUBLISHED
var stateOrder = []string{"PENDING", "VALIDATING", "VALIDATED", "PUBLISHING", "PUBLISHED"}

type Deployment struct {
	ID             string   `json:"deploymentId"`
	Name           string   `json:"deploymentName"`
	State          string   `json:"deploymentState"`
	PublishingType string   `json:"publishingType"`
	Purls          []string `json:"purls"`
	SavedPath      string   `json:"-"`
}

var (
	mu          sync.Mutex
	deployments = make(map[string]*Deployment)
)

func advanceState(dep *Deployment) {
	for i, s := range stateOrder {
		if s == dep.State && i < len(stateOrder)-1 {
			dep.State = stateOrder[i+1]
			return
		}
	}
}

func autoPublish(deploymentID string) {
	for range len(stateOrder) {
		time.Sleep(1 * time.Second)
		mu.Lock()
		dep, ok := deployments[deploymentID]
		if !ok {
			mu.Unlock()
			return
		}
		if dep.State == "PUBLISHED" {
			mu.Unlock()
			return
		}
		advanceState(dep)
		slog.Info("Auto-advance", "id", deploymentID, "state", dep.State)
		mu.Unlock()
	}
}

func requireAuth(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	return len(token) > 0
}

func writeText(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprint(w, msg)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// POST /api/v1/publisher/upload
func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeText(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if !requireAuth(r) {
		writeText(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	if err := r.ParseMultipartForm(256 << 20); err != nil {
		writeText(w, http.StatusBadRequest, "Failed to parse multipart form")
		return
	}

	file, header, err := r.FormFile("bundle")
	if err != nil {
		writeText(w, http.StatusBadRequest, "Missing 'bundle' file field")
		return
	}
	defer file.Close()

	name := r.URL.Query().Get("name")
	if name == "" {
		name = header.Filename
		if name == "" {
			name = "unnamed"
		}
	}

	publishingType := r.URL.Query().Get("publishingType")
	if publishingType == "" {
		publishingType = "USER_MANAGED"
	}
	if publishingType != "AUTOMATIC" && publishingType != "USER_MANAGED" {
		writeText(w, http.StatusBadRequest, "Invalid publishingType; must be AUTOMATIC or USER_MANAGED")
		return
	}

	// Save to temp dir.
	tmpDir, err := os.MkdirTemp("", "mock-central-")
	if err != nil {
		writeText(w, http.StatusInternalServerError, "Failed to create temp dir")
		return
	}
	filename := header.Filename
	if filename == "" {
		filename = "bundle.zip"
	}
	dest := filepath.Join(tmpDir, filename)
	out, err := os.Create(dest)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "Failed to create temp file")
		return
	}
	fileSize, err := io.Copy(out, file)
	out.Close()
	if err != nil {
		writeText(w, http.StatusInternalServerError, "Failed to save file")
		return
	}

	deploymentID := newUUID()

	mu.Lock()
	deployments[deploymentID] = &Deployment{
		ID:             deploymentID,
		Name:           name,
		State:          "PENDING",
		PublishingType: publishingType,
		Purls:          []string{},
		SavedPath:      dest,
	}
	mu.Unlock()

	slog.Info("Upload received",
		"id", deploymentID,
		"name", name,
		"size", fileSize,
		"publishingType", publishingType,
	)

	if publishingType == "AUTOMATIC" {
		go autoPublish(deploymentID)
	}

	writeText(w, http.StatusCreated, deploymentID)
}

// POST /api/v1/publisher/status?id=...
func handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeText(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if !requireAuth(r) {
		writeText(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	deploymentID := r.URL.Query().Get("id")
	if deploymentID == "" {
		writeText(w, http.StatusBadRequest, "Missing 'id' query parameter")
		return
	}

	mu.Lock()
	dep, ok := deployments[deploymentID]
	if !ok {
		mu.Unlock()
		writeText(w, http.StatusNotFound, "Not found")
		return
	}

	if dep.PublishingType == "AUTOMATIC" {
		advanceState(dep)
		slog.Info("Status poll (AUTOMATIC)", "id", deploymentID, "state", dep.State)
	} else {
		if dep.State == "PENDING" || dep.State == "VALIDATING" {
			advanceState(dep)
			slog.Info("Status poll (USER_MANAGED)", "id", deploymentID, "state", dep.State)
		}
	}

	resp := map[string]any{
		"deploymentId":    dep.ID,
		"deploymentName":  dep.Name,
		"deploymentState": dep.State,
		"purls":           dep.Purls,
	}
	mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

// POST /api/v1/publisher/deployment/{id}  — publish
// DELETE /api/v1/publisher/deployment/{id} — drop
func handleDeployment(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(r) {
		writeText(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	deploymentID := strings.TrimPrefix(r.URL.Path, "/api/v1/publisher/deployment/")
	if deploymentID == "" {
		writeText(w, http.StatusBadRequest, "Missing deployment ID")
		return
	}

	mu.Lock()
	dep, ok := deployments[deploymentID]
	if !ok {
		mu.Unlock()
		writeText(w, http.StatusNotFound, "Not found")
		return
	}

	switch r.Method {
	case http.MethodPost:
		// Publish
		if dep.State != "VALIDATED" {
			msg := fmt.Sprintf("Cannot publish: deployment is in %s state", dep.State)
			mu.Unlock()
			writeText(w, http.StatusBadRequest, msg)
			return
		}
		dep.State = "PUBLISHING"
		slog.Info("Publish triggered", "id", deploymentID)
		mu.Unlock()

		go func() {
			time.Sleep(1 * time.Second)
			mu.Lock()
			d, ok := deployments[deploymentID]
			if ok && d.State == "PUBLISHING" {
				d.State = "PUBLISHED"
				slog.Info("Deployment is now PUBLISHED", "id", deploymentID)
			}
			mu.Unlock()
		}()

		w.WriteHeader(http.StatusNoContent)

	case http.MethodDelete:
		// Drop
		if dep.State != "VALIDATED" && dep.State != "FAILED" {
			msg := fmt.Sprintf("Cannot drop: deployment is in %s state", dep.State)
			mu.Unlock()
			writeText(w, http.StatusBadRequest, msg)
			return
		}
		delete(deployments, deploymentID)
		slog.Info("Dropped deployment", "id", deploymentID)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		mu.Unlock()
		writeText(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// GET /health — health check endpoint
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeText(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GET /deployments — list all deployments (test observability endpoint)
func handleListDeployments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeText(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	mu.Lock()
	result := make([]map[string]any, 0, len(deployments))
	for _, dep := range deployments {
		result = append(result, map[string]any{
			"deploymentId":    dep.ID,
			"deploymentName":  dep.Name,
			"deploymentState": dep.State,
			"publishingType":  dep.PublishingType,
			"purls":           dep.Purls,
		})
	}
	mu.Unlock()

	writeJSON(w, http.StatusOK, result)
}

func main() {
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/api/v1/publisher/upload", handleUpload)
	http.HandleFunc("/api/v1/publisher/status", handleStatus)
	http.HandleFunc("/api/v1/publisher/deployment/", handleDeployment)
	http.HandleFunc("/deployments", handleListDeployments)

	slog.Info("Mock Maven Central starting on 0.0.0.0:8082")
	if err := http.ListenAndServe(":8082", nil); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}
