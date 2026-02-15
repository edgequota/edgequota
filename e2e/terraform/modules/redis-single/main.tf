# --------------------------------------------------------------------------
# Redis Single — one standalone instance
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

resource "kubernetes_deployment_v1" "redis" {
  metadata {
    name      = "redis-single"
    namespace = var.namespace
    labels    = { app = "redis-single" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 1

    selector {
      match_labels = { app = "redis-single" }
    }

    template {
      metadata {
        labels = { app = "redis-single" }
      }

      spec {
        container {
          name  = "redis"
          image = "redis:7-alpine"

          port {
            container_port = 6379
          }

          resources {
            requests = { cpu = "100m", memory = "64Mi" }
            limits   = { cpu = "500m", memory = "128Mi" }
          }

          readiness_probe {
            exec {
              command = ["redis-cli", "ping"]
            }
            initial_delay_seconds = 5
            period_seconds        = 5
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "redis" {
  metadata {
    name      = "redis-single"
    namespace = var.namespace
  }

  spec {
    selector = { app = "redis-single" }

    port {
      port        = 6379
      target_port = 6379
    }
  }
}

output "endpoint" {
  value = "redis-single.${var.namespace}.svc.cluster.local:6379"
}
