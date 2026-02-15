# --------------------------------------------------------------------------
# Redis Cluster — 6-node cluster (3 masters + 3 replicas)
# Uses a StatefulSet so each pod gets a stable network identity required
# by the cluster gossip protocol. A Job initializes the cluster topology.
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

resource "kubernetes_config_map_v1" "redis_conf" {
  metadata {
    name      = "redis-cluster-config"
    namespace = var.namespace
  }

  data = {
    "redis.conf" = <<-EOT
      port 6379
      cluster-enabled yes
      cluster-config-file /data/nodes.conf
      cluster-node-timeout 5000
      appendonly yes
    EOT
  }
}

resource "kubernetes_stateful_set_v1" "cluster" {
  metadata {
    name      = "redis-cluster"
    namespace = var.namespace
    labels    = { app = "redis-cluster" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    service_name = "redis-cluster"
    replicas     = 6

    selector {
      match_labels = { app = "redis-cluster" }
    }

    template {
      metadata {
        labels = { app = "redis-cluster" }
      }

      spec {
        container {
          name    = "redis"
          image   = "redis:7-alpine"
          command = ["redis-server", "/etc/redis/redis.conf"]

          port {
            container_port = 6379
            name           = "client"
          }

          port {
            container_port = 16379
            name           = "gossip"
          }

          volume_mount {
            name       = "config"
            mount_path = "/etc/redis"
          }

          volume_mount {
            name       = "data"
            mount_path = "/data"
          }

          readiness_probe {
            exec { command = ["redis-cli", "ping"] }
            initial_delay_seconds = 10
            period_seconds        = 5
          }

          resources {
            requests = { cpu = "100m", memory = "64Mi" }
            limits   = { cpu = "500m", memory = "128Mi" }
          }
        }

        volume {
          name = "config"

          config_map {
            name = kubernetes_config_map_v1.redis_conf.metadata[0].name
          }
        }

        volume {
          name = "data"
          empty_dir {}
        }
      }
    }
  }
}

# Headless service for stable pod DNS entries (required for cluster gossip).
resource "kubernetes_service_v1" "headless" {
  metadata {
    name      = "redis-cluster"
    namespace = var.namespace
  }

  spec {
    cluster_ip = "None"
    selector   = { app = "redis-cluster" }

    port {
      port        = 6379
      target_port = 6379
      name        = "client"
    }

    port {
      port        = 16379
      target_port = 16379
      name        = "gossip"
    }
  }
}

# Job that waits for all 6 nodes then creates the cluster.
resource "kubernetes_job_v1" "cluster_init" {
  metadata {
    name      = "redis-cluster-init"
    namespace = var.namespace
  }

  timeouts {
    create = "2m"
  }

  spec {
    backoff_limit = 5

    template {
      metadata {
        labels = { app = "redis-cluster-init" }
      }

      spec {
        restart_policy = "OnFailure"

        container {
          name  = "init"
          image = "redis:7-alpine"

          command = [
            "sh", "-c",
            <<-EOT
              set -e
              NS="${var.namespace}"

              echo "Waiting for all 6 Redis cluster nodes..."
              for i in 0 1 2 3 4 5; do
                HOST="redis-cluster-$i.redis-cluster.$NS.svc.cluster.local"
                echo "  Waiting for $HOST ..."
                until redis-cli -h "$HOST" ping 2>/dev/null | grep -q PONG; do
                  sleep 2
                done
                echo "  ✓ $HOST ready"
              done

              echo "Creating cluster..."
              redis-cli --cluster create \
                redis-cluster-0.redis-cluster.$NS.svc.cluster.local:6379 \
                redis-cluster-1.redis-cluster.$NS.svc.cluster.local:6379 \
                redis-cluster-2.redis-cluster.$NS.svc.cluster.local:6379 \
                redis-cluster-3.redis-cluster.$NS.svc.cluster.local:6379 \
                redis-cluster-4.redis-cluster.$NS.svc.cluster.local:6379 \
                redis-cluster-5.redis-cluster.$NS.svc.cluster.local:6379 \
                --cluster-replicas 1 \
                --cluster-yes

              echo "Cluster created successfully."
            EOT
          ]
        }
      }
    }
  }

  depends_on = [kubernetes_stateful_set_v1.cluster]
}

output "endpoints" {
  value = [
    for i in range(6) :
    "redis-cluster-${i}.redis-cluster.${var.namespace}.svc.cluster.local:6379"
  ]
}
