resource "google_cloudfunctions2_function" "webhook_receiver" {
  name     = "maven-sync-webhook"
  location = var.region

  description = "Receives Nexus 3 webhooks and publishes sync jobs to Pub/Sub"

  build_config {
    runtime     = "go123"
    entry_point = "HandleWebhook"

    source {
      storage_source {
        bucket = google_storage_bucket.source.name
        object = google_storage_bucket_object.webhook_source.name
      }
    }
  }

  service_config {
    max_instance_count = 10
    min_instance_count = 0
    available_memory   = "256Mi"
    timeout_seconds    = 30

    service_account_email = google_service_account.webhook_receiver.email

    environment_variables = {
      PUBSUB_TOPIC = google_pubsub_topic.maven_sync.id
      GCP_PROJECT  = var.project_id
    }

    secret_environment_variables {
      key        = "WEBHOOK_SECRET"
      project_id = var.project_id
      secret     = google_secret_manager_secret.webhook_secret.secret_id
      version    = "latest"
    }
  }

  labels = {
    app = "maven-sync"
  }
}

# Allow unauthenticated access (Nexus webhook can't use GCP IAM)
resource "google_cloud_run_service_iam_member" "webhook_public" {
  location = var.region
  service  = google_cloudfunctions2_function.webhook_receiver.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# Source bucket for Cloud Function code
resource "google_storage_bucket" "source" {
  name     = "${var.project_id}-maven-sync-source"
  location = var.region

  uniform_bucket_level_access = true

  labels = {
    app = "maven-sync"
  }
}

resource "google_storage_bucket_object" "webhook_source" {
  name   = "webhook-receiver-source.zip"
  bucket = google_storage_bucket.source.name
  source = "${path.module}/../cmd/webhook-receiver/function.zip"
}
