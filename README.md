# maven-central-sync

Syncs Maven artifacts from Sonatype Nexus 3 to Maven Central.

Nexus 3 stores artifacts as binary blobs, which breaks the rsync-based sync that Maven Central previously used with Nexus 2. This tool replaces that workflow using Nexus 3 webhooks and the Maven Central Publisher API.

Written in Go — stdlib only for core logic, no external dependencies except GCP client libraries for the production path.

## Architecture

Two deployment modes: a monolith for local dev/testing, and a pub/sub architecture for production.

### Local Dev (monolith)

```
Nexus 3 ──webhook──▶ sync-service ──fetch──▶ Nexus 3
                          │
                          └──bundle+upload──▶ mock Maven Central
```

### Production (GCP pub/sub)

```
Nexus 3 (your-nexus.example.com)
  │
  │ webhook (HMAC-signed)
  ▼
Cloud Function (webhook-receiver)     ← validates, publishes to Pub/Sub
  │
  ▼
Cloud Pub/Sub (maven-sync topic)      ← durable, 7-day retention, DLQ
  │
  │ push subscription
  ▼
Cloud Run (publisher-service)         ← auto-scales 0→50, concurrency=1
  │
  ├─ fetch from Nexus (Search Assets API)
  ├─ bundle into ZIP (Maven repo layout)
  └─ upload to Maven Central (Publisher API)
```

**Why pub/sub?** Maven artifact publishing is bursty (thousands of artifacts at once). Pub/Sub provides:
- **Durability** — messages survive service crashes, 7-day retention
- **Fan-out** — Cloud Run auto-scales horizontally on queue depth
- **Backpressure** — if Maven Central is slow, messages queue instead of timing out
- **Retry + DLQ** — failed syncs retry with exponential backoff, then land in dead letter for investigation
- **IAM** — all service-to-service auth via GCP service accounts, no API keys

## Quick Start (Local Dev)

### Prerequisites

- Docker and Docker Compose
- bash, jq (for setup scripts)

### Start the Stack

```bash
docker compose up -d
```

This starts three services:
- **nexus** (port 8081) — Sonatype Nexus 3 repository manager
- **sync-service** (port 8080) — webhook receiver + artifact syncer
- **mock-central** (port 8082) — mock Maven Central Publisher API

Nexus takes 2-3 minutes to start on first launch.

### Configure Nexus

```bash
./scripts/setup-nexus.sh
```

### Run End-to-End Test

```bash
./scripts/e2e-test.sh
```

### Deploy a Test Artifact

```bash
./scripts/deploy-test-artifact.sh 2.0.0
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `NEXUS_URL` | `http://nexus:8081` | Nexus 3 base URL |
| `NEXUS_USERNAME` | `admin` | Nexus admin username |
| `NEXUS_PASSWORD` | `admin123` | Nexus admin password |
| `MAVEN_CENTRAL_URL` | `http://mock-central:8082` | Maven Central Publisher API URL |
| `MAVEN_CENTRAL_TOKEN` | `test-token` | Bearer token for Maven Central |
| `WEBHOOK_SECRET` | _(empty)_ | HMAC-SHA1 secret for webhook validation |
| `WORKER_CONCURRENCY` | `3` | Concurrent sync workers (monolith only) |
| `FETCH_RETRY_TIMEOUT` | `60s` | Max time to wait for Nexus to index assets |
| `HTTP_TIMEOUT` | `120s` | HTTP client timeout |
| `PUBLISH_TIMEOUT` | `300s` | Max time to wait for Maven Central publication |
| `PUBLISH_POLL_INTERVAL` | `5s` | Polling interval for publication status |

## Production Deployment (GCP)

### Prerequisites

- GCP project with Pub/Sub, Cloud Functions, Cloud Run, Secret Manager APIs enabled
- Terraform >= 1.5

### Deploy Infrastructure

```bash
cd terraform
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars with your GCP project details
terraform init
terraform plan
terraform apply
```

### Set Secrets

```bash
echo -n "your-nexus-password" | gcloud secrets versions add maven-sync-nexus-password --data-file=-
echo -n "base64(user:pass)" | gcloud secrets versions add maven-sync-maven-central-token --data-file=-
echo -n "your-webhook-secret" | gcloud secrets versions add maven-sync-webhook-secret --data-file=-
```

### Build and Deploy Publisher Service

```bash
# Build and push to Artifact Registry
docker build -f cmd/publisher-service/Dockerfile -t ${REGION}-docker.pkg.dev/${PROJECT}/maven-sync/publisher-service:latest .
docker push ${REGION}-docker.pkg.dev/${PROJECT}/maven-sync/publisher-service:latest

# Deploy Cloud Run
gcloud run deploy maven-sync-publisher --image=${REGION}-docker.pkg.dev/${PROJECT}/maven-sync/publisher-service:latest --region=${REGION}
```

### Configure Nexus Webhook

Point the Nexus webhook to the Cloud Function URL (from `terraform output webhook_url`).

## Project Structure

```
go.work                              # Go workspace (all modules)
internal/sync/                       # Shared library — fetcher + publisher logic
  fetcher.go                         #   Download artifacts from Nexus Search Assets API
  publisher.go                       #   Bundle ZIP + upload to Central Publisher API
sync-service/                        # Monolith for local dev (webhook + worker pool)
  main.go                            #   HTTP server, HMAC validation, async dispatch
  worker.go                          #   Job queue with deduplication
mock-central/                        # Mock Maven Central Publisher API
  main.go                            #   Upload, status, publish, drop endpoints
cmd/
  webhook-receiver/                  # GCP Cloud Function — Nexus webhook → Pub/Sub
    function.go
  publisher-service/                 # GCP Cloud Run — Pub/Sub → fetch → publish
    main.go
    Dockerfile
terraform/                           # GCP infrastructure (Pub/Sub, Cloud Run, IAM, Secrets)
scripts/
  setup-nexus.sh                     # One-time Nexus setup
  deploy-test-artifact.sh            # Deploy test artifact to Nexus
  e2e-test.sh                        # Full end-to-end test
```
