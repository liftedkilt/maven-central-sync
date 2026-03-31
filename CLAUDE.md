# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Purpose

Designed for organizations migrating from Nexus 2 to Nexus 3. Maven Central previously rsynced from Nexus 2, but Nexus 3's blob storage broke that. This is the replacement: webhook-driven artifact sync from Nexus 3 to Maven Central.

## Architecture

Two deployment modes sharing the same core logic (`internal/sync/`):

**Local dev (monolith):** `sync-service/` — receives Nexus webhooks, async worker pool fetches + publishes. Docker Compose with Nexus 3, sync-service, and mock Maven Central.

**Production (GCP pub/sub):** Nexus webhook → Cloud Function (`cmd/webhook-receiver/`) → Pub/Sub → Cloud Run (`cmd/publisher-service/`). Auto-scales 0→50 instances, concurrency=1 per instance. IAM for all service-to-service auth. Terraform in `terraform/`.

## Build and Run

```bash
# Go workspace — build all modules
cd internal/sync && go build ./...
cd sync-service && go build ./...
cd mock-central && go build ./...
cd cmd/publisher-service && go build ./...
cd cmd/webhook-receiver && go vet ./...  # Cloud Function, not main package

# Tests
cd internal/sync && go test ./...       # Fetcher + publisher tests
cd sync-service && go test ./...        # Webhook, HMAC, dedup, worker tests
cd mock-central && go test ./...        # Mock Central state machine tests

# Docker (local dev)
docker compose up -d
./scripts/setup-nexus.sh
./scripts/e2e-test.sh
```

## Module Structure (Go workspace)

- `internal/sync/` — package `mavensync`. Shared fetcher (Nexus Search Assets API) and publisher (Central Publisher API). Imported by both sync-service and Cloud Run publisher.
- `sync-service/` — Monolith: webhook handler with HMAC validation, async worker pool with dedup, imports `mavensync`.
- `mock-central/` — Mock Central Publisher API with state machine.
- `cmd/webhook-receiver/` — Cloud Function (package `webhookreceiver`). Validates webhook, publishes to Pub/Sub.
- `cmd/publisher-service/` — Cloud Run service. Receives Pub/Sub push, calls `mavensync.FetchComponentAssets` + `mavensync.Publish`.

## Key APIs

- **Nexus Search Assets**: `GET /service/rest/v1/search/assets?repository=...&maven.groupId=...&maven.artifactId=...&maven.baseVersion=...`
- **Central Publisher Upload**: `POST /api/v1/publisher/upload` (multipart ZIP)
- **Central Publisher Status**: `POST /api/v1/publisher/status?id={deploymentId}`
- **Nexus Webhook**: `{"action":"CREATED","component":{"format":"maven2","group":"...","name":"...","version":"..."}}`
- **Pub/Sub push envelope**: `{"message":{"data":"base64json","attributes":{...},"messageId":"..."}}`

## Environment Variables

NEXUS_URL, NEXUS_USERNAME, NEXUS_PASSWORD, MAVEN_CENTRAL_URL, MAVEN_CENTRAL_TOKEN, WEBHOOK_SECRET, WORKER_CONCURRENCY, FETCH_RETRY_TIMEOUT, HTTP_TIMEOUT, PUBLISH_TIMEOUT, PUBLISH_POLL_INTERVAL, PORT. See `.env.example`.
