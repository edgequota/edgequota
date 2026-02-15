# --------------------------------------------------------------------------
# Redis Sentinel — 1 primary + 2 replicas + 3 sentinel instances
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

# --- Primary ---

resource "kubernetes_deployment_v1" "primary" {
  metadata {
    name      = "redis-sentinel-primary"
    namespace = var.namespace
    labels    = { app = "redis-sentinel", role = "master" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 1

    selector {
      match_labels = { app = "redis-sentinel", role = "master" }
    }

    template {
      metadata {
        labels = { app = "redis-sentinel", role = "master" }
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
    name      = "redis-sentinel-primary"
    namespace = var.namespace
  }

  spec {
    selector = { app = "redis-sentinel", role = "master" }

    port {
      port        = 6379
      target_port = 6379
    }
  }
}

# --- Replicas ---

resource "kubernetes_deployment_v1" "replica" {
  metadata {
    name      = "redis-sentinel-replica"
    namespace = var.namespace
    labels    = { app = "redis-sentinel", role = "replica" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 2

    selector {
      match_labels = { app = "redis-sentinel", role = "replica" }
    }

    template {
      metadata {
        labels = { app = "redis-sentinel", role = "replica" }
      }

      spec {
        container {
          name    = "redis"
          image   = "redis:7-alpine"
          command = ["redis-server", "--replicaof", "redis-sentinel-primary.${var.namespace}.svc.cluster.local", "6379"]

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

# --- Sentinel configuration ---

resource "kubernetes_config_map_v1" "sentinel_conf" {
  metadata {
    name      = "sentinel-config"
    namespace = var.namespace
  }

  data = {
    "sentinel.conf" = <<-EOT
      port 26379
      sentinel resolve-hostnames yes
      sentinel monitor mymaster redis-sentinel-primary.${var.namespace}.svc.cluster.local 6379 2
      sentinel down-after-milliseconds mymaster 5000
      sentinel failover-timeout mymaster 10000
      sentinel parallel-syncs mymaster 1
    EOT
  }
}

# --- Sentinel instances ---
# Sentinel rewrites its config file, so we copy the template to an emptyDir
# via an init container and let sentinel mutate it freely.

resource "kubernetes_deployment_v1" "sentinel" {
  metadata {
    name      = "redis-sentinel-node"
    namespace = var.namespace
    labels    = { app = "redis-sentinel-node" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 3

    selector {
      match_labels = { app = "redis-sentinel-node" }
    }

    template {
      metadata {
        labels = { app = "redis-sentinel-node" }
      }

      spec {
        init_container {
          name  = "copy-config"
          image = "redis:7-alpine"
          command = [
            "sh", "-c",
            "cp /tmp/sentinel.conf /etc/redis/sentinel.conf"
          ]

          volume_mount {
            name       = "sentinel-template"
            mount_path = "/tmp/sentinel.conf"
            sub_path   = "sentinel.conf"
          }

          volume_mount {
            name       = "sentinel-data"
            mount_path = "/etc/redis"
          }
        }

        container {
          name    = "sentinel"
          image   = "redis:7-alpine"
          command = ["redis-sentinel", "/etc/redis/sentinel.conf"]

          port {
            container_port = 26379
          }

          volume_mount {
            name       = "sentinel-data"
            mount_path = "/etc/redis"
          }

          readiness_probe {
            exec {
              command = ["redis-cli", "-p", "26379", "ping"]
            }
            initial_delay_seconds = 10
            period_seconds        = 5
          }
        }

        volume {
          name = "sentinel-template"

          config_map {
            name = kubernetes_config_map_v1.sentinel_conf.metadata[0].name
          }
        }

        volume {
          name = "sentinel-data"
          empty_dir {}
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "sentinel" {
  metadata {
    name      = "redis-sentinel"
    namespace = var.namespace
  }

  spec {
    selector = { app = "redis-sentinel-node" }

    port {
      port        = 26379
      target_port = 26379
    }
  }
}

output "sentinel_endpoint" {
  value = "redis-sentinel.${var.namespace}.svc.cluster.local:26379"
}
