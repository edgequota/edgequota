# --------------------------------------------------------------------------
# Namespace
# --------------------------------------------------------------------------
resource "kubernetes_namespace_v1" "e2e" {
  metadata {
    name = var.namespace
  }
}

# --------------------------------------------------------------------------
# Redis topologies
# --------------------------------------------------------------------------
module "redis_single" {
  source    = "./modules/redis-single"
  namespace = kubernetes_namespace_v1.e2e.metadata[0].name
}

module "redis_replication" {
  source    = "./modules/redis-replication"
  namespace = kubernetes_namespace_v1.e2e.metadata[0].name
}

module "redis_sentinel" {
  source    = "./modules/redis-sentinel"
  namespace = kubernetes_namespace_v1.e2e.metadata[0].name
}

module "redis_cluster" {
  source    = "./modules/redis-cluster"
  namespace = kubernetes_namespace_v1.e2e.metadata[0].name
}

# --------------------------------------------------------------------------
# Backend services
# --------------------------------------------------------------------------
module "whoami" {
  source    = "./modules/whoami"
  namespace = kubernetes_namespace_v1.e2e.metadata[0].name
}

module "testbackend" {
  source    = "./modules/testbackend"
  namespace = kubernetes_namespace_v1.e2e.metadata[0].name
  image     = var.testbackend_image
}

# --------------------------------------------------------------------------
# Locals — shared config fragments
# --------------------------------------------------------------------------
module "tls_certs" {
  source    = "./modules/tls-certs"
  namespace = kubernetes_namespace_v1.e2e.metadata[0].name
}

locals {
  ns                  = kubernetes_namespace_v1.e2e.metadata[0].name
  backend_url         = module.whoami.endpoint
  testbackend_url     = module.testbackend.endpoint
  testbackend_tls_url = module.testbackend.tls_endpoint

  # Redis endpoints for each topology.
  redis_single_ep    = module.redis_single.endpoint
  redis_repl_primary = module.redis_replication.primary_endpoint
  redis_repl_all     = module.redis_replication.all_endpoint
  redis_sentinel_ep  = module.redis_sentinel.sentinel_endpoint
  redis_cluster_eps  = module.redis_cluster.endpoints
}

# --------------------------------------------------------------------------
# EdgeQuota scenarios — one module instance per test scenario
# --------------------------------------------------------------------------

# --- single-pt: Redis single, passThrough ---
module "eq_single_pt" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "single-pt"
  image     = var.edgequota_image
  node_port = 30101

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "single-pt"
      static:
        backend_url: "${local.backend_url}"
        average: 100
        burst: 10
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami]
}

# --- single-fc: Redis single, failClosed ---
module "eq_single_fc" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "single-fc"
  image     = var.edgequota_image
  node_port = 30102

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "failClosed"
      key_prefix: "single-fc"
      static:
        backend_url: "${local.backend_url}"
        average: 100
        burst: 10
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami]
}

# --- single-fb: Redis single, inMemoryFallback ---
module "eq_single_fb" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "single-fb"
  image     = var.edgequota_image
  node_port = 30103

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "inMemoryFallback"
      key_prefix: "single-fb"
      static:
        backend_url: "${local.backend_url}"
        average: 100
        burst: 10
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami]
}

# --- repl-basic: Redis replication, passThrough ---
module "eq_repl_basic" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "repl-basic"
  image     = var.edgequota_image
  node_port = 30104

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "repl-basic"
      static:
        backend_url: "${local.backend_url}"
        average: 100
        burst: 10
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_repl_primary}"
        - "${local.redis_repl_all}"
      mode: "replication"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_replication, module.whoami]
}

# --- sentinel-basic: Redis sentinel, passThrough ---
module "eq_sentinel_basic" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "sentinel-basic"
  image     = var.edgequota_image
  node_port = 30105

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "sentinel-basic"
      static:
        backend_url: "${local.backend_url}"
        average: 100
        burst: 10
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_sentinel_ep}"
      mode: "sentinel"
      master_name: "mymaster"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_sentinel, module.whoami]
}

# --- cluster-basic: Redis cluster, passThrough ---
module "eq_cluster_basic" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "cluster-basic"
  image     = var.edgequota_image
  node_port = 30106

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "cluster-basic"
      static:
        backend_url: "${local.backend_url}"
        average: 100
        burst: 10
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
%{for ep in local.redis_cluster_eps~}
        - "${ep}"
%{endfor~}
      mode: "cluster"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_cluster, module.whoami]
}

# --- key-header: Redis single, header key strategy ---
module "eq_key_header" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "key-header"
  image     = var.edgequota_image
  node_port = 30107

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "key-header"
      static:
        backend_url: "${local.backend_url}"
        average: 100
        burst: 10
        period: "1s"
        key_strategy:
          type: "header"
          header_name: "X-Tenant-Id"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami]
}

# --- key-composite: Redis single, composite key strategy ---
module "eq_key_composite" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "key-composite"
  image     = var.edgequota_image
  node_port = 30108

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "key-composite"
      static:
        backend_url: "${local.backend_url}"
        average: 100
        burst: 10
        period: "1s"
        key_strategy:
          type: "composite"
          header_name: "X-Tenant-Id"
          path_prefix: true
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami]
}

# --- burst-test: Redis single, low burst for testing exhaustion ---
module "eq_burst_test" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "burst-test"
  image     = var.edgequota_image
  node_port = 30109

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "burst-test"
      static:
        backend_url: "${local.backend_url}"
        average: 2
        burst: 3
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami]
}

# --- no-limit: Redis single, average=0 (disabled rate limiting) ---
module "eq_no_limit" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "no-limit"
  image     = var.edgequota_image
  node_port = 30110

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "no-limit"
      static:
        backend_url: "${local.backend_url}"
        average: 0
        burst: 1
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami]
}

# --------------------------------------------------------------------------
# Protocol test scenarios (multi-protocol testbackend)
# --------------------------------------------------------------------------

# --- protocol: no rate limit — validates gRPC, SSE, WebSocket pass-through ---
module "eq_protocol" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "protocol"
  image     = var.edgequota_image
  node_port = 30111

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "60s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "30s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "protocol"
      static:
        backend_url: "${local.testbackend_url}"
        average: 0
        burst: 1
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.testbackend]
}

# --- protocol-rl: WITH rate limit — validates gRPC/SSE/WS get 429 when limited ---
module "eq_protocol_rl" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "protocol-rl"
  image     = var.edgequota_image
  node_port = 30112

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "60s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "30s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "protocol-rl"
      static:
        backend_url: "${local.testbackend_url}"
        average: 2
        burst: 3
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.testbackend]
}

# --------------------------------------------------------------------------
# HTTP/3 protocol test scenario (TLS + QUIC)
# --------------------------------------------------------------------------

# --- protocol-h3: TLS + HTTP/3 enabled, no rate limit ---
module "eq_protocol_h3" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "protocol-h3"
  image     = var.edgequota_image
  node_port = 30113

  tls_secret_name = module.tls_certs.secret_name

  extra_ports = [
    {
      name        = "tls"
      port        = 8443
      target_port = 8443
      protocol    = "TCP"
      node_port   = 30214
    },
    {
      name        = "quic"
      port        = 8443
      target_port = 8443
      protocol    = "UDP"
      node_port   = 30214
    },
  ]

  config_yaml = <<-YAML
    server:
      address: ":8443"
      read_timeout: "30s"
      write_timeout: "60s"
      idle_timeout: "120s"
      drain_timeout: "5s"
      tls:
        enabled: true
        cert_file: "/etc/edgequota/tls/tls.crt"
        key_file: "/etc/edgequota/tls/tls.key"
        http3_enabled: true
    admin:
      address: ":9090"
    backend:
      timeout: "30s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      tls_insecure_skip_verify: true
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "protocol-h3"
      static:
        backend_url: "${local.testbackend_tls_url}"
        average: 0
        burst: 1
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.testbackend, module.tls_certs]
}

# --- config-reload: Test hot-reload of rate limit parameters ---
module "eq_config_reload" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "config-reload"
  image     = var.edgequota_image
  node_port = 30115

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "config-reload"
      static:
        backend_url: "${local.backend_url}"
        average: 100
        burst: 50
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami]
}

# --------------------------------------------------------------------------
# Mock external rate limit service (for tenant-aware backend URL tests)
# --------------------------------------------------------------------------
module "mockextrl" {
  source        = "./modules/mockextrl"
  namespace     = local.ns
  image         = var.mockextrl_image
  backend_a_url = local.backend_url
  backend_b_url = local.testbackend_url
  node_port     = 30190
}

# --- dynamic-backend: Tenant-aware backend URL via external RL service ---
module "eq_dynamic_backend" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "dynamic-backend"
  image     = var.edgequota_image
  node_port = 30116
  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      transport:
        backend_protocol: "h1"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "dynamic-backend"
      static:
        backend_url: "${local.backend_url}"
        average: 1000
        burst: 500
        period: "1s"
        key_strategy:
          type: "header"
          header_name: "X-Tenant-Id"
      external:
        enabled: true
        timeout: "5s"
        fallback:
          backend_url: "${local.backend_url}"
          average: 1000
          burst: 500
          period: "1s"
          key_strategy:
            type: "global"
            global_key: "dynamic-fallback"
        http:
          url: "${module.mockextrl.endpoint}"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami, module.testbackend, module.mockextrl]
}

# --------------------------------------------------------------------------
# Cache (CDN-style response cache) test scenarios
# --------------------------------------------------------------------------

# --- cache-basic: Response cache enabled, testbackend returns Cache-Control ---
module "eq_cache_basic" {
  source          = "./modules/edgequota"
  namespace       = local.ns
  scenario        = "cache-basic"
  image           = var.edgequota_image
  node_port       = 30117
  admin_node_port = 30217

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      url_policy:
        deny_private_networks: false
    cache:
      enabled: true
      max_body_size: "1MB"
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "cache-basic"
      static:
        backend_url: "${local.testbackend_url}"
        average: 0
        burst: 1
        period: "1s"
        key_strategy:
          type: "clientIP"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.testbackend]
}

# --- cache-extrl: External RL with caching (tests RL response cache) ---
module "eq_cache_extrl" {
  source    = "./modules/edgequota"
  namespace = local.ns
  scenario  = "cache-extrl"
  image     = var.edgequota_image
  node_port = 30118

  config_yaml = <<-YAML
    server:
      address: ":8080"
      read_timeout: "30s"
      write_timeout: "30s"
      idle_timeout: "120s"
      drain_timeout: "5s"
    admin:
      address: ":9090"
    backend:
      timeout: "10s"
      max_idle_conns: 50
      idle_conn_timeout: "60s"
      transport:
        backend_protocol: "h1"
      url_policy:
        deny_private_networks: false
    rate_limit:
      failure_policy: "passThrough"
      key_prefix: "cache-extrl"
      static:
        backend_url: "${local.backend_url}"
        average: 1000
        burst: 500
        period: "1s"
        key_strategy:
          type: "header"
          header_name: "X-Tenant-Id"
      external:
        enabled: true
        timeout: "5s"
        fallback:
          backend_url: "${local.backend_url}"
          average: 1000
          burst: 500
          period: "1s"
          key_strategy:
            type: "global"
            global_key: "cache-extrl-fallback"
        http:
          url: "${module.mockextrl.endpoint}"
    redis:
      endpoints:
        - "${local.redis_single_ep}"
      mode: "single"
      pool_size: 5
      dial_timeout: "3s"
      read_timeout: "2s"
      write_timeout: "2s"
    logging:
      level: "debug"
      format: "json"
  YAML

  depends_on = [module.redis_single, module.whoami, module.mockextrl]
}
