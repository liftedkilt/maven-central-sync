// Package webhookreceiver is a GCP Cloud Function (Gen2) that receives
// Nexus 3 webhook events and publishes artifact details to Cloud Pub/Sub.
package webhookreceiver

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"cloud.google.com/go/pubsub"
)

var (
	pubsubClient *pubsub.Client
	pubsubOnce   sync.Once
	initErr      error
)

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

// pubsubMessage is the JSON structure published to Pub/Sub.
type pubsubMessage struct {
	Repository string `json:"repository"`
	GroupID    string `json:"groupId"`
	ArtifactID string `json:"artifactId"`
	Version    string `json:"version"`
}

func initPubSubClient() {
	project := os.Getenv("GCP_PROJECT")
	if project == "" {
		initErr = fmt.Errorf("GCP_PROJECT environment variable is not set")
		return
	}

	pubsubClient, initErr = pubsub.NewClient(context.Background(), project)
}

// HandleWebhook is the Cloud Function entry point.
func HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// Read raw body for HMAC validation and JSON parsing.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Failed to read request body", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}

	// HMAC-SHA1 signature validation (if secret is configured).
	if secret := os.Getenv("WEBHOOK_SECRET"); secret != "" {
		sig := r.Header.Get("X-Nexus-Webhook-Signature")
		mac := hmac.New(sha1.New, []byte(secret))
		mac.Write(bodyBytes)
		expected := hex.EncodeToString(mac.Sum(nil))

		if !hmac.Equal([]byte(expected), []byte(sig)) {
			slog.Warn("Invalid webhook signature")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
			return
		}
	}

	// Parse the webhook payload.
	var payload webhookPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		slog.Error("Invalid JSON payload", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	// Filter: only process CREATED events for maven2 artifacts.
	if payload.Action != "CREATED" || payload.Component.Format != "maven2" {
		slog.Info("Ignoring event",
			"action", payload.Action,
			"format", payload.Component.Format,
		)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	// Initialize Pub/Sub client (once).
	pubsubOnce.Do(initPubSubClient)
	if initErr != nil {
		slog.Error("Pub/Sub client initialization failed", "error", initErr)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "pubsub init failed"})
		return
	}

	topicName := os.Getenv("PUBSUB_TOPIC")
	if topicName == "" {
		slog.Error("PUBSUB_TOPIC environment variable is not set")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "pubsub topic not configured"})
		return
	}

	// Build the Pub/Sub message.
	msg := pubsubMessage{
		Repository: payload.RepositoryName,
		GroupID:    payload.Component.Group,
		ArtifactID: payload.Component.Name,
		Version:    payload.Component.Version,
	}

	msgData, err := json.Marshal(msg)
	if err != nil {
		slog.Error("Failed to marshal message", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to marshal message"})
		return
	}

	orderingKey := fmt.Sprintf("%s:%s:%s", msg.GroupID, msg.ArtifactID, msg.Version)

	topic := pubsubClient.Topic(topicName)
	topic.EnableMessageOrdering = true

	result := topic.Publish(r.Context(), &pubsub.Message{
		Data:        msgData,
		OrderingKey: orderingKey,
		Attributes: map[string]string{
			"repository": msg.Repository,
			"groupId":    msg.GroupID,
			"artifactId": msg.ArtifactID,
			"version":    msg.Version,
		},
	})

	// Wait for the publish to complete.
	serverID, err := result.Get(r.Context())
	if err != nil {
		slog.Error("Failed to publish message",
			"error", err,
			"groupId", msg.GroupID,
			"artifactId", msg.ArtifactID,
			"version", msg.Version,
		)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to publish message"})
		return
	}

	slog.Info("Published message to Pub/Sub",
		"serverID", serverID,
		"repository", msg.Repository,
		"groupId", msg.GroupID,
		"artifactId", msg.ArtifactID,
		"version", msg.Version,
	)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":    "accepted",
		"component": orderingKey,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
