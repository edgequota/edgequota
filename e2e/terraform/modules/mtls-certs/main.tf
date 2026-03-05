# --------------------------------------------------------------------------
# Client CA + client certificate for mTLS e2e testing
# --------------------------------------------------------------------------

variable "namespace" {
  type = string
}

variable "name" {
  description = "Name of the Kubernetes Secret holding the client CA bundle"
  type        = string
  default     = "edgequota-mtls-client-ca"
}

# --- Client CA ---

resource "tls_private_key" "client_ca" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "client_ca" {
  private_key_pem = tls_private_key.client_ca.private_key_pem

  subject {
    common_name  = "EdgeQuota E2E Client CA"
    organization = "EdgeQuota"
  }

  validity_period_hours = 8760
  is_ca_certificate     = true

  allowed_uses = [
    "cert_signing",
    "digital_signature",
    "key_encipherment",
  ]
}

# --- Client certificate signed by the client CA ---

resource "tls_private_key" "client" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_cert_request" "client" {
  private_key_pem = tls_private_key.client.private_key_pem

  subject {
    common_name  = "e2e-test-device"
    organization = "EdgeQuota E2E"
    serial_number = "42"
  }
}

resource "tls_locally_signed_cert" "client" {
  cert_request_pem   = tls_cert_request.client.cert_request_pem
  ca_private_key_pem = tls_private_key.client_ca.private_key_pem
  ca_cert_pem        = tls_self_signed_cert.client_ca.cert_pem

  validity_period_hours = 8760

  allowed_uses = [
    "digital_signature",
    "key_encipherment",
    "client_auth",
  ]
}

# --- Kubernetes Secret for the client CA (mounted by EdgeQuota) ---

resource "kubernetes_secret_v1" "client_ca" {
  metadata {
    name      = var.name
    namespace = var.namespace
  }

  data = {
    "ca.crt" = tls_self_signed_cert.client_ca.cert_pem
  }
}

# --- Outputs ---

output "secret_name" {
  value = kubernetes_secret_v1.client_ca.metadata[0].name
}

output "client_ca_cert_pem" {
  value     = tls_self_signed_cert.client_ca.cert_pem
  sensitive = true
}

output "client_cert_pem" {
  value     = tls_locally_signed_cert.client.cert_pem
  sensitive = true
}

output "client_key_pem" {
  value     = tls_private_key.client.private_key_pem
  sensitive = true
}
