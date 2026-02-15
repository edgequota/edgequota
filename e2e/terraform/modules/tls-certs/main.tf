# --------------------------------------------------------------------------
# Self-signed TLS certificates for e2e testing (stored as K8s Secret)
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

variable "name" {
  description = "Name of the Kubernetes Secret"
  type        = string
  default     = "edgequota-tls"
}

resource "tls_private_key" "ca" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "ca" {
  private_key_pem = tls_private_key.ca.private_key_pem

  subject {
    common_name  = "EdgeQuota E2E CA"
    organization = "EdgeQuota"
  }

  validity_period_hours = 8760 # 1 year
  is_ca_certificate     = true

  allowed_uses = [
    "cert_signing",
    "digital_signature",
    "key_encipherment",
  ]
}

resource "tls_private_key" "server" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_cert_request" "server" {
  private_key_pem = tls_private_key.server.private_key_pem

  subject {
    common_name  = "edgequota-protocol-h3"
    organization = "EdgeQuota"
  }

  dns_names = [
    "edgequota-protocol-h3",
    "edgequota-protocol-h3.${var.namespace}.svc.cluster.local",
    "localhost",
  ]

  ip_addresses = ["127.0.0.1"]
}

resource "tls_locally_signed_cert" "server" {
  cert_request_pem   = tls_cert_request.server.cert_request_pem
  ca_private_key_pem = tls_private_key.ca.private_key_pem
  ca_cert_pem        = tls_self_signed_cert.ca.cert_pem

  validity_period_hours = 8760

  allowed_uses = [
    "digital_signature",
    "key_encipherment",
    "server_auth",
  ]
}

resource "kubernetes_secret_v1" "tls" {
  metadata {
    name      = var.name
    namespace = var.namespace
  }

  type = "kubernetes.io/tls"

  data = {
    "tls.crt" = tls_locally_signed_cert.server.cert_pem
    "tls.key" = tls_private_key.server.private_key_pem
    "ca.crt"  = tls_self_signed_cert.ca.cert_pem
  }
}

output "secret_name" {
  value = kubernetes_secret_v1.tls.metadata[0].name
}

output "ca_cert_pem" {
  value     = tls_self_signed_cert.ca.cert_pem
  sensitive = true
}
