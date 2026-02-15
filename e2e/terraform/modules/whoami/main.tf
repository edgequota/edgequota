# --------------------------------------------------------------------------
# Whoami — simple HTTP backend that echoes request info
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

resource "kubernetes_deployment_v1" "whoami" {
  metadata {
    name      = "whoami"
    namespace = var.namespace
    labels    = { app = "whoami" }
  }

  timeouts {
    create = "2m"
    update = "2m"
  }

  spec {
    replicas = 2

    selector {
      match_labels = { app = "whoami" }
    }

    template {
      metadata {
        labels = { app = "whoami" }
      }

      spec {
        container {
          name  = "whoami"
          image = "traefik/whoami:latest"

          port {
            container_port = 80
          }

          readiness_probe {
            http_get {
              path = "/health"
              port = 80
            }
            initial_delay_seconds = 2
            period_seconds        = 5
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "whoami" {
  metadata {
    name      = "whoami"
    namespace = var.namespace
  }

  spec {
    selector = { app = "whoami" }

    port {
      port        = 80
      target_port = 80
    }
  }
}

output "endpoint" {
  value = "http://whoami.${var.namespace}.svc.cluster.local:80"
}
