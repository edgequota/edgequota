# --------------------------------------------------------------------------
# Multi-protocol test backend (HTTP + gRPC/h2c + SSE + WebSocket + HTTP/3)
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

variable "image" {
  description = "Docker image for the test backend"
  type        = string
}

resource "kubernetes_deployment_v1" "testbackend" {
  metadata {
    name      = "testbackend"
    namespace = var.namespace
    labels    = { app = "testbackend" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 2

    selector {
      match_labels = { app = "testbackend" }
    }

    template {
      metadata {
        labels = { app = "testbackend" }
      }

      spec {
        container {
          name              = "testbackend"
          image             = var.image
          image_pull_policy = "Never"

          port {
            container_port = 8080
            name           = "cleartext"
          }

          port {
            container_port = 8443
            name           = "tls"
            protocol       = "TCP"
          }

          port {
            container_port = 8443
            name           = "quic"
            protocol       = "UDP"
          }

          readiness_probe {
            http_get {
              path = "/health"
              port = 8080
            }
            initial_delay_seconds = 2
            period_seconds        = 5
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "testbackend" {
  metadata {
    name      = "testbackend"
    namespace = var.namespace
  }

  spec {
    selector = { app = "testbackend" }

    port {
      name        = "cleartext"
      port        = 8080
      target_port = 8080
      protocol    = "TCP"
    }

    port {
      name        = "tls"
      port        = 8443
      target_port = 8443
      protocol    = "TCP"
    }

    port {
      name        = "quic"
      port        = 8443
      target_port = 8443
      protocol    = "UDP"
    }
  }
}

output "endpoint" {
  value = "http://testbackend.${var.namespace}.svc.cluster.local:8080"
}

output "tls_endpoint" {
  value = "https://testbackend.${var.namespace}.svc.cluster.local:8443"
}
