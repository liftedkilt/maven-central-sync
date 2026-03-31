output "webhook_url" {
  description = "URL to configure as Nexus webhook endpoint"
  value       = google_cloudfunctions2_function.webhook_receiver.url
}

output "publisher_service_url" {
  description = "Cloud Run publisher service URL (internal)"
  value       = google_cloud_run_v2_service.publisher.uri
}

output "pubsub_topic" {
  description = "Pub/Sub topic for sync jobs"
  value       = google_pubsub_topic.maven_sync.id
}

output "dlq_topic" {
  description = "Dead letter queue topic for failed syncs"
  value       = google_pubsub_topic.maven_sync_dlq.id
}

output "artifact_registry" {
  description = "Docker image registry for publisher service"
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.maven_sync.repository_id}"
}
