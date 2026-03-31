# Main topic — webhook receiver publishes here
resource "google_pubsub_topic" "maven_sync" {
  name = "maven-sync"

  message_retention_duration = var.message_retention

  labels = {
    app = "maven-sync"
  }
}

# Dead letter topic — failed messages land here
resource "google_pubsub_topic" "maven_sync_dlq" {
  name = "maven-sync-dlq"

  message_retention_duration = var.message_retention

  labels = {
    app     = "maven-sync"
    purpose = "dead-letter"
  }
}

# Push subscription — delivers to Cloud Run publisher service
resource "google_pubsub_subscription" "maven_sync_push" {
  name  = "maven-sync-push"
  topic = google_pubsub_topic.maven_sync.id

  ack_deadline_seconds = var.pubsub_ack_deadline

  push_config {
    push_endpoint = google_cloud_run_v2_service.publisher.uri

    oidc_token {
      service_account_email = google_service_account.pubsub_invoker.email
    }

    attributes = {
      x-goog-version = "v1"
    }
  }

  dead_letter_policy {
    dead_letter_topic     = google_pubsub_topic.maven_sync_dlq.id
    max_delivery_attempts = var.pubsub_retry_count
  }

  retry_policy {
    minimum_backoff = "10s"
    maximum_backoff = "300s"
  }

  # Enable exactly-once delivery for deduplication
  enable_exactly_once_delivery = true

  labels = {
    app = "maven-sync"
  }
}

# DLQ subscription for monitoring/investigation
resource "google_pubsub_subscription" "maven_sync_dlq_sub" {
  name  = "maven-sync-dlq-sub"
  topic = google_pubsub_topic.maven_sync_dlq.id

  ack_deadline_seconds       = 60
  message_retention_duration = var.message_retention

  labels = {
    app     = "maven-sync"
    purpose = "dead-letter"
  }
}
