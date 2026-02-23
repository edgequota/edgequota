# --------------------------------------------------------------------------
# Mock external rate limit service for tenant-aware backend URL E2E tests.
# Returns per-tenant backend_url based on the rate limit key.
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

variable "image" {
  description = "Docker image for the mock external RL service"
  type        = string
}

variable "backend_a_url" {
  description = "Backend URL returned for tenant-a"
  type        = string
}

variable "backend_b_url" {
  description = "Backend URL returned for tenant-b"
  type        = string
}

variable "node_port" {
  description = "Optional NodePort for external access (0 = ClusterIP only)"
  type        = number
  default     = 0
}

resource "kubernetes_deployment_v1" "mockextrl" {
  metadata {
    name      = "mockextrl"
    namespace = var.namespace
    labels    = { app = "mockextrl" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 1

    selector {
      match_labels = { app = "mockextrl" }
    }

    template {
      metadata {
        labels = { app = "mockextrl" }
      }

      spec {
        container {
          name              = "mockextrl"
          image             = var.image
          image_pull_policy = "Never"

          port {
            container_port = 8090
            name           = "http"
          }

          env {
            name  = "BACKEND_A_URL"
            value = var.backend_a_url
          }

          env {
            name  = "BACKEND_B_URL"
            value = var.backend_b_url
          }

          readiness_probe {
            http_get {
              path = "/health"
              port = 8090
            }
            initial_delay_seconds = 2
            period_seconds        = 5
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "mockextrl" {
  metadata {
    name      = "mockextrl"
    namespace = var.namespace
  }

  spec {
    type     = var.node_port > 0 ? "NodePort" : "ClusterIP"
    selector = { app = "mockextrl" }

    port {
      port        = 8090
      target_port = 8090
      node_port   = var.node_port > 0 ? var.node_port : null
    }
  }
}

output "endpoint" {
  value = "http://mockextrl.${var.namespace}.svc.cluster.local:8090"
}
