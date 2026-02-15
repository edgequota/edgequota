# --------------------------------------------------------------------------
# Redis Replication — 1 primary + 2 replicas
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

# --- Primary ---

resource "kubernetes_deployment_v1" "primary" {
  metadata {
    name      = "redis-repl-primary"
    namespace = var.namespace
    labels    = { app = "redis-replication", role = "master" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 1

    selector {
      match_labels = { app = "redis-replication", role = "master" }
    }

    template {
      metadata {
        labels = { app = "redis-replication", role = "master" }
      }

      spec {
        container {
          name  = "redis"
          image = "redis:7-alpine"

          port {
            container_port = 6379
          }

          readiness_probe {
            exec { command = ["redis-cli", "ping"] }
            initial_delay_seconds = 5
            period_seconds        = 5
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "primary" {
  metadata {
    name      = "redis-repl-primary"
    namespace = var.namespace
  }

  spec {
    selector = { app = "redis-replication", role = "master" }

    port {
      port        = 6379
      target_port = 6379
    }
  }
}

# --- Replicas ---

resource "kubernetes_deployment_v1" "replica" {
  metadata {
    name      = "redis-repl-replica"
    namespace = var.namespace
    labels    = { app = "redis-replication", role = "replica" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 2

    selector {
      match_labels = { app = "redis-replication", role = "replica" }
    }

    template {
      metadata {
        labels = { app = "redis-replication", role = "replica" }
      }

      spec {
        container {
          name    = "redis"
          image   = "redis:7-alpine"
          command = ["redis-server", "--replicaof", "redis-repl-primary.${var.namespace}.svc.cluster.local", "6379"]

          port {
            container_port = 6379
          }

          readiness_probe {
            exec { command = ["redis-cli", "ping"] }
            initial_delay_seconds = 5
            period_seconds        = 5
          }
        }
      }
    }
  }

  depends_on = [kubernetes_service_v1.primary]
}

# Service that load-balances across all pods (primary + replicas).
resource "kubernetes_service_v1" "all" {
  metadata {
    name      = "redis-repl-all"
    namespace = var.namespace
  }

  spec {
    selector = { app = "redis-replication" }

    port {
      port        = 6379
      target_port = 6379
    }
  }
}

output "primary_endpoint" {
  value = "redis-repl-primary.${var.namespace}.svc.cluster.local:6379"
}

output "all_endpoint" {
  value = "redis-repl-all.${var.namespace}.svc.cluster.local:6379"
}
