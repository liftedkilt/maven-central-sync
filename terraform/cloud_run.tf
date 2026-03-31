resource "google_cloud_run_v2_service" "publisher" {
  name     = "maven-sync-publisher"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_INTERNAL_ONLY"

  description = "Fetches artifacts from Nexus and publishes to Maven Central"

  template {
    service_account = google_service_account.publisher.email

    scaling {
      min_instance_count = 0
      max_instance_count = var.publisher_max_instances
    }

    timeout = "${var.publisher_timeout}s"

    # One sync job per instance — each is IO-heavy
    max_instance_request_concurrency = 1

    containers {
      image = "${var.region}-docker.pkg.dev/${var.project_id}/maven-sync/publisher-service:latest"

      resources {
        limits = {
          cpu    = "1"
          memory = "1Gi"
        }
      }

      env {
        name  = "NEXUS_URL"
        value = var.nexus_url
      }

      env {
        name  = "NEXUS_USERNAME"
        value = var.nexus_username
      }

      env {
        name  = "MAVEN_CENTRAL_URL"
        value = var.maven_central_url
      }

      env {
        name = "NEXUS_PASSWORD"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.nexus_password.secret_id
            version = "latest"
          }
        }
      }

      env {
        name = "MAVEN_CENTRAL_TOKEN"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.maven_central_token.secret_id
            version = "latest"
          }
        }
      }

      env {
        name  = "FETCH_RETRY_TIMEOUT"
        value = "120s"
      }

      env {
        name  = "PUBLISH_TIMEOUT"
        value = "300s"
      }
    }
  }

  labels = {
    app = "maven-sync"
  }
}

# Artifact Registry for Docker images
resource "google_artifact_registry_repository" "maven_sync" {
  location      = var.region
  repository_id = "maven-sync"
  format        = "DOCKER"
  description   = "Container images for Maven Sync services"

  labels = {
    app = "maven-sync"
  }
}
