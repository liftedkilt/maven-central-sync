resource "google_secret_manager_secret" "webhook_secret" {
  secret_id = "maven-sync-webhook-secret"

  replication {
    auto {}
  }

  labels = {
    app = "maven-sync"
  }
}

resource "google_secret_manager_secret" "nexus_password" {
  secret_id = "maven-sync-nexus-password"

  replication {
    auto {}
  }

  labels = {
    app = "maven-sync"
  }
}

resource "google_secret_manager_secret" "maven_central_token" {
  secret_id = "maven-sync-maven-central-token"

  replication {
    auto {}
  }

  labels = {
    app = "maven-sync"
  }
}
