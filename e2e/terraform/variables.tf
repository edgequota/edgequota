variable "namespace" {
  description = "Kubernetes namespace for all E2E resources"
  type        = string
  default     = "edgequota-e2e"
}

variable "edgequota_image" {
  description = "Docker image for EdgeQuota (pre-loaded into minikube)"
  type        = string
  default     = "edgequota:e2e"
}

variable "testbackend_image" {
  description = "Docker image for the multi-protocol test backend (pre-loaded into minikube)"
  type        = string
  default     = "testbackend:e2e"
}

variable "mockextrl_image" {
  description = "Docker image for the mock external rate limit service (pre-loaded into minikube)"
  type        = string
  default     = "mockextrl:e2e"
}
