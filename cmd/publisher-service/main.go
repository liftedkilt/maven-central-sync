package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mavensync "github.com/liftedkilt/maven-central-sync/internal/sync"
)

type pubSubMessage struct {
	Attributes map[string]string `json:"attributes"`
	Data       string            `json:"data"`
	MessageID  string            `json:"messageId"`
}

type pubSubEnvelope struct {
	Message      pubSubMessage `json:"message"`
	Subscription string        `json:"subscription"`
}

type artifactData struct {
	Repository string `json:"repository"`
	GroupID    string `json:"groupId"`
	ArtifactID string `json:"artifactId"`
	Version    string `json:"version"`
}

type config struct {
	NexusURL            string
	NexusUsername       string
	NexusPassword       string
	MavenCentralURL     string
	MavenCentralToken   string
	FetchRetryTimeout   time.Duration
	HTTPTimeout         time.Duration
	PublishTimeout      time.Duration
	PublishPollInterval time.Duration
	Port                string
}

func loadConfig() config {
	return config{
		NexusURL:            requireEnv("NEXUS_URL"),
		NexusUsername:       requireEnv("NEXUS_USERNAME"),
		NexusPassword:       requireEnv("NEXUS_PASSWORD"),
		MavenCentralURL:     requireEnv("MAVEN_CENTRAL_URL"),
		MavenCentralToken:   requireEnv("MAVEN_CENTRAL_TOKEN"),
		FetchRetryTimeout:   parseDuration("FETCH_RETRY_TIMEOUT", 120*time.Second),
		HTTPTimeout:         parseDuration("HTTP_TIMEOUT", 120*time.Second),
		PublishTimeout:      parseDuration("PUBLISH_TIMEOUT", 300*time.Second),
		PublishPollInterval: parseDuration("PUBLISH_POLL_INTERVAL", 10*time.Second),
		Port:                getEnv("PORT", "8080"),
	}
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Warn("Required environment variable not set", "key", key)
	}

	return v
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func parseDuration(key string, fallback time.Duration) time.Duration {
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

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := loadConfig()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", handlePubSub(cfg))
	mux.HandleFunc("GET /health", handleHealth)

	server := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: mux,
	}

	// Graceful shutdown on SIGTERM (Cloud Run sends this).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		slog.Info("Starting publisher service", "port", cfg.Port)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("Shutting down gracefully")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("Shutdown error", "error", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func handlePubSub(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var envelope pubSubEnvelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			slog.Error("Failed to decode Pub/Sub envelope", "error", err)
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		messageID := envelope.Message.MessageID
		logger := slog.With("messageId", messageID)

		// Decode the base64 data field to get artifact coordinates.
		var artifact artifactData

		if envelope.Message.Data != "" {
			decoded, err := base64.StdEncoding.DecodeString(envelope.Message.Data)
			if err != nil {
				logger.Error("Failed to decode base64 message data", "error", err)
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			if err := json.Unmarshal(decoded, &artifact); err != nil {
				logger.Error("Failed to parse artifact JSON from data field", "error", err)
				w.WriteHeader(http.StatusBadRequest)

				return
			}
		}

		// Fall back to attributes if data fields are empty.
		if artifact.Repository == "" {
			artifact.Repository = envelope.Message.Attributes["repository"]
		}

		if artifact.GroupID == "" {
			artifact.GroupID = envelope.Message.Attributes["groupId"]
		}

		if artifact.ArtifactID == "" {
			artifact.ArtifactID = envelope.Message.Attributes["artifactId"]
		}

		if artifact.Version == "" {
			artifact.Version = envelope.Message.Attributes["version"]
		}

		logger = logger.With(
			"repository", artifact.Repository,
			"groupId", artifact.GroupID,
			"artifactId", artifact.ArtifactID,
			"version", artifact.Version,
		)

		if artifact.GroupID == "" || artifact.ArtifactID == "" || artifact.Version == "" {
			logger.Error("Missing required artifact coordinates")
			// Ack to avoid infinite retries on malformed messages.
			w.WriteHeader(http.StatusOK)

			return
		}

		logger.Info("Processing artifact sync")

		client := &http.Client{Timeout: cfg.HTTPTimeout}

		// Fetch all assets from Nexus.
		tempDir, files, err := mavensync.FetchComponentAssets(
			cfg.NexusURL, cfg.NexusUsername, cfg.NexusPassword,
			artifact.Repository, artifact.GroupID, artifact.ArtifactID, artifact.Version,
			client, cfg.FetchRetryTimeout,
		)

		if tempDir != "" {
			defer os.RemoveAll(tempDir)
		}

		if err != nil {
			logger.Error("Failed to fetch assets", "error", err)
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		if len(files) == 0 {
			logger.Warn("No assets found, acknowledging message")
			w.WriteHeader(http.StatusOK)

			return
		}

		logger.Info("Fetched assets", "count", len(files), "tempDir", tempDir)

		// Publish to Maven Central.
		status, err := mavensync.Publish(
			tempDir, cfg.MavenCentralURL, cfg.MavenCentralToken,
			client, cfg.PublishTimeout, cfg.PublishPollInterval,
		)

		if err != nil {
			// Check for permanent errors (400-level from Maven Central) to avoid retrying.
			if isPermanentError(err) {
				logger.Error("Permanent publish error, acknowledging to prevent retries", "error", err)
				w.WriteHeader(http.StatusOK)

				return
			}

			logger.Error("Failed to publish", "error", err)
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		finalState, _ := status["deploymentState"].(string)
		if finalState == "FAILED" {
			logger.Error("Deployment failed on Maven Central", "status", status)
			// Ack to prevent retries — the artifact itself is problematic.
			w.WriteHeader(http.StatusOK)

			return
		}

		logger.Info("Successfully published artifact", "state", finalState)
		w.WriteHeader(http.StatusOK)
	}
}

// isPermanentError checks if an error indicates a non-retryable condition
// (e.g., 400 Bad Request from Maven Central).
func isPermanentError(err error) bool {
	msg := err.Error()

	return strings.Contains(msg, "status 400") ||
		strings.Contains(msg, "status 401") ||
		strings.Contains(msg, "status 403") ||
		strings.Contains(msg, "status 422")
}
