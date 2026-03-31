# Service account for the webhook receiver Cloud Function
resource "google_service_account" "webhook_receiver" {
  account_id   = "maven-sync-webhook"
  display_name = "Maven Sync Webhook Receiver"
  description  = "Cloud Function that receives Nexus webhooks and publishes to Pub/Sub"
}

# Service account for the publisher Cloud Run service
resource "google_service_account" "publisher" {
  account_id   = "maven-sync-publisher"
  display_name = "Maven Sync Publisher"
  description  = "Cloud Run service that fetches from Nexus and publishes to Maven Central"
}

# Service account for Pub/Sub to invoke Cloud Run
resource "google_service_account" "pubsub_invoker" {
  account_id   = "maven-sync-pubsub-invoker"
  display_name = "Maven Sync Pub/Sub Invoker"
  description  = "Allows Pub/Sub to push messages to Cloud Run"
}

# Webhook receiver can publish to Pub/Sub topic
resource "google_pubsub_topic_iam_member" "webhook_publisher" {
  topic  = google_pubsub_topic.maven_sync.id
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:${google_service_account.webhook_receiver.email}"
}

# Pub/Sub invoker can invoke Cloud Run
resource "google_cloud_run_v2_service_iam_member" "pubsub_invoker" {
  name     = google_cloud_run_v2_service.publisher.name
  location = var.region
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.pubsub_invoker.email}"
}

# Pub/Sub service agent needs to create tokens for the invoker SA
resource "google_project_iam_member" "pubsub_token_creator" {
  project = var.project_id
  role    = "roles/iam.serviceAccountTokenCreator"
  member  = "serviceAccount:service-${data.google_project.current.number}@gcp-sa-pubsub.iam.gserviceaccount.com"
}

# Publisher SA can access secrets
resource "google_secret_manager_secret_iam_member" "publisher_nexus_password" {
  secret_id = google_secret_manager_secret.nexus_password.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.publisher.email}"
}

resource "google_secret_manager_secret_iam_member" "publisher_maven_central_token" {
  secret_id = google_secret_manager_secret.maven_central_token.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.publisher.email}"
}

resource "google_secret_manager_secret_iam_member" "webhook_secret_accessor" {
  secret_id = google_secret_manager_secret.webhook_secret.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.webhook_receiver.email}"
}

# Allow Pub/Sub SA to publish to DLQ
resource "google_pubsub_topic_iam_member" "dlq_publisher" {
  topic  = google_pubsub_topic.maven_sync_dlq.id
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:service-${data.google_project.current.number}@gcp-sa-pubsub.iam.gserviceaccount.com"
}

# Allow Pub/Sub SA to subscribe (needed for DLQ forwarding)
resource "google_pubsub_subscription_iam_member" "dlq_subscriber" {
  subscription = google_pubsub_subscription.maven_sync_push.id
  role         = "roles/pubsub.subscriber"
  member       = "serviceAccount:service-${data.google_project.current.number}@gcp-sa-pubsub.iam.gserviceaccount.com"
}

data "google_project" "current" {}
