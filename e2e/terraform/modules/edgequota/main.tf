# --------------------------------------------------------------------------
# EdgeQuota instance — one Deployment + ConfigMap + NodePort Service
# per test scenario. This module is instantiated once for each scenario.
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

variable "scenario" {
  description = "Unique scenario identifier (e.g. single-pt, burst-test)"
  type        = string
}

variable "image" {
  description = "EdgeQuota container image"
  type        = string
}

variable "node_port" {
  description = "NodePort to expose this instance on"
  type        = number
}

variable "config_yaml" {
  description = "Full EdgeQuota YAML configuration for this scenario"
  type        = string
}

variable "tls_secret_name" {
  description = "Optional: name of a TLS Secret to mount at /etc/edgequota/tls"
  type        = string
  default     = ""
}

variable "extra_ports" {
  description = "Additional port blocks for the service (e.g. UDP for QUIC)"
  type = list(object({
    name        = string
    port        = number
    target_port = number
    protocol    = string
    node_port   = optional(number)
  }))
  default = []
}

# --- ConfigMap ---

resource "kubernetes_config_map_v1" "config" {
  metadata {
    name      = "edgequota-${var.scenario}"
    namespace = var.namespace
  }

  data = {
    "config.yaml" = var.config_yaml
  }
}

# --- Deployment ---

resource "kubernetes_deployment_v1" "edgequota" {
  metadata {
    name      = "edgequota-${var.scenario}"
    namespace = var.namespace
    labels = {
      app                  = "edgequota"
      "edgequota-scenario" = var.scenario
    }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 1

    selector {
      match_labels = {
        app                  = "edgequota"
        "edgequota-scenario" = var.scenario
      }
    }

    template {
      metadata {
        labels = {
          app                  = "edgequota"
          "edgequota-scenario" = var.scenario
        }
      }

      spec {
        container {
          name              = "edgequota"
          image             = var.image
          image_pull_policy = "Never" # Loaded via minikube image load

          args = ["-config", "/etc/edgequota/config.yaml"]

          port {
            container_port = 8080
            name           = "proxy"
          }

          port {
            container_port = 9090
            name           = "admin"
          }

          # Extra ports for TLS/QUIC scenarios.
          dynamic "port" {
            for_each = var.extra_ports
            content {
              container_port = port.value.target_port
              name           = port.value.name
              protocol       = port.value.protocol
            }
          }

          env {
            name  = "EDGEQUOTA_CONFIG_FILE"
            value = "/etc/edgequota/config.yaml"
          }

          volume_mount {
            name       = "config"
            mount_path = "/etc/edgequota"
            read_only  = true
          }

          # Conditional TLS volume mount.
          dynamic "volume_mount" {
            for_each = var.tls_secret_name != "" ? [1] : []
            content {
              name       = "tls-certs"
              mount_path = "/etc/edgequota/tls"
              read_only  = true
            }
          }

          startup_probe {
            http_get {
              path = "/startz"
              port = 9090
            }
            initial_delay_seconds = 2
            period_seconds        = 3
            failure_threshold     = 10
          }

          readiness_probe {
            http_get {
              path = "/readyz"
              port = 9090
            }
            period_seconds = 5
          }

          liveness_probe {
            http_get {
              path = "/healthz"
              port = 9090
            }
            period_seconds = 10
          }

          resources {
            requests = { cpu = "50m", memory = "32Mi" }
            limits   = { cpu = "500m", memory = "128Mi" }
          }
        }

        volume {
          name = "config"

          config_map {
            name = kubernetes_config_map_v1.config.metadata[0].name
          }
        }

        # Conditional TLS volume.
        dynamic "volume" {
          for_each = var.tls_secret_name != "" ? [1] : []
          content {
            name = "tls-certs"

            secret {
              secret_name = var.tls_secret_name
            }
          }
        }
      }
    }
  }
}

# --- NodePort Service ---

resource "kubernetes_service_v1" "edgequota" {
  metadata {
    name      = "edgequota-${var.scenario}"
    namespace = var.namespace
    labels = {
      app                  = "edgequota"
      "edgequota-scenario" = var.scenario
    }
  }

  spec {
    type = "NodePort"
    selector = {
      app                  = "edgequota"
      "edgequota-scenario" = var.scenario
    }

    port {
      name        = "proxy"
      port        = 8080
      target_port = 8080
      node_port   = var.node_port
    }

    # Extra service ports (e.g. TLS + QUIC for HTTP/3).
    dynamic "port" {
      for_each = var.extra_ports
      content {
        name        = port.value.name
        port        = port.value.port
        target_port = port.value.target_port
        protocol    = port.value.protocol
        node_port   = port.value.node_port
      }
    }
  }
}

# --- Admin Service (ClusterIP, for internal health checks) ---

resource "kubernetes_service_v1" "admin" {
  metadata {
    name      = "edgequota-${var.scenario}-admin"
    namespace = var.namespace
  }

  spec {
    selector = {
      app                  = "edgequota"
      "edgequota-scenario" = var.scenario
    }

    port {
      name        = "admin"
      port        = 9090
      target_port = 9090
    }
  }
}

output "node_port" {
  value = var.node_port
}

output "service_name" {
  value = "edgequota-${var.scenario}"
}
