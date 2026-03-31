package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Config holds environment-based configuration.
type Config struct {
	NexusURL           string
	NexusUsername       string
	NexusPassword      string
	MavenCentralURL    string
	MavenCentralToken  string
	Port               string
	WebhookSecret      string
	WorkerConcurrency  int
	FetchRetryTimeout  time.Duration
	HTTPTimeout        time.Duration
	PublishTimeout     time.Duration
	PublishPollInterval time.Duration
}

func loadConfig() Config {
	return Config{
		NexusURL:           envOrDefault("NEXUS_URL", "http://localhost:8081"),
		NexusUsername:       os.Getenv("NEXUS_USERNAME"),
		NexusPassword:      os.Getenv("NEXUS_PASSWORD"),
		MavenCentralURL:    envOrDefault("MAVEN_CENTRAL_URL", "http://localhost:8082"),
		MavenCentralToken:  os.Getenv("MAVEN_CENTRAL_TOKEN"),
		Port:               envOrDefault("PORT", "8080"),
		WebhookSecret:      os.Getenv("WEBHOOK_SECRET"),
		WorkerConcurrency:  envInt("WORKER_CONCURRENCY", 3),
		FetchRetryTimeout:  envDuration("FETCH_RETRY_TIMEOUT", 60*time.Second),
		HTTPTimeout:        envDuration("HTTP_TIMEOUT", 120*time.Second),
		PublishTimeout:     envDuration("PUBLISH_TIMEOUT", 300*time.Second),
		PublishPollInterval: envDuration("PUBLISH_POLL_INTERVAL", 5*time.Second),
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("Invalid duration, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}

	return d
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("Invalid integer, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}

	return n
}

// webhookPayload represents the Nexus 3 webhook JSON body.
type webhookPayload struct {
	Action         string `json:"action"`
	RepositoryName string `json:"repositoryName"`
	Component      struct {
		Format  string `json:"format"`
		Group   string `json:"group"`
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"component"`
}

func main() {
	cfg := loadConfig()

	worker := NewWorker(cfg)
	worker.Start()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", webhookHandler(cfg, worker))
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /statuses", statusesHandler(worker))

	addr := ":" + cfg.Port
	slog.Info("Sync service starting", "addr", addr)

	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
}

func webhookHandler(cfg Config, worker *Worker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Read raw body for HMAC validation and JSON parsing.
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
			return
		}

		// HMAC-SHA1 signature validation.
		if cfg.WebhookSecret != "" {
			sig := r.Header.Get("X-Nexus-Webhook-Signature")
			mac := hmac.New(sha1.New, []byte(cfg.WebhookSecret))
			mac.Write(bodyBytes)
			expected := hex.EncodeToString(mac.Sum(nil))

			if sig != expected {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
				return
			}
		}

		var payload webhookPayload
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}

		if payload.Action != "CREATED" || payload.Component.Format != "maven2" {
			slog.Info("Ignoring event",
				"action", payload.Action,
				"format", payload.Component.Format,
			)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}

		job := SyncJob{
			Repository: payload.RepositoryName,
			GroupID:    payload.Component.Group,
			ArtifactID: payload.Component.Name,
			Version:    payload.Component.Version,
		}

		gav := gavKey(job.GroupID, job.ArtifactID, job.Version)
		result := worker.Enqueue(job)

		if result == "duplicate" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
			return
		}

		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":    "accepted",
			"component": gav,
		})
	}
}

func statusesHandler(worker *Worker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, worker.ListStatuses())
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// verifyHMAC is exported for testing.
func verifyHMAC(body []byte, secret, signature string) bool {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}
