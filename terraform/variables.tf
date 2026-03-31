variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "region" {
  description = "GCP region"
  type        = string
  default     = "us-central1"
}

variable "nexus_url" {
  description = "Nexus 3 base URL (e.g., https://nexus.example.com)"
  type        = string
}

variable "nexus_username" {
  description = "Nexus admin username for artifact downloads"
  type        = string
  default     = "admin"
}

variable "maven_central_url" {
  description = "Maven Central Publisher API URL"
  type        = string
  default     = "https://central.sonatype.com"
}

variable "publisher_max_instances" {
  description = "Max Cloud Run instances for publisher service"
  type        = number
  default     = 50
}

variable "publisher_timeout" {
  description = "Cloud Run request timeout (seconds)"
  type        = number
  default     = 600
}

variable "pubsub_retry_count" {
  description = "Max delivery attempts before sending to DLQ"
  type        = number
  default     = 5
}

variable "pubsub_ack_deadline" {
  description = "Pub/Sub ack deadline in seconds (must exceed publisher timeout)"
  type        = number
  default     = 600
}

variable "message_retention" {
  description = "Pub/Sub message retention duration"
  type        = string
  default     = "604800s"  # 7 days
}
