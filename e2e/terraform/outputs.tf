output "namespace" {
  value = var.namespace
}

# Redis endpoints for debugging.
output "redis_single_endpoint" {
  value = module.redis_single.endpoint
}

output "redis_replication_primary_endpoint" {
  value = module.redis_replication.primary_endpoint
}

output "redis_replication_all_endpoint" {
  value = module.redis_replication.all_endpoint
}

output "redis_sentinel_endpoint" {
  value = module.redis_sentinel.sentinel_endpoint
}

output "redis_cluster_endpoints" {
  value = module.redis_cluster.endpoints
}

# EdgeQuota NodePorts per scenario.
output "eq_single_pt_port" {
  value = module.eq_single_pt.node_port
}

output "eq_single_fc_port" {
  value = module.eq_single_fc.node_port
}

output "eq_single_fb_port" {
  value = module.eq_single_fb.node_port
}

output "eq_repl_basic_port" {
  value = module.eq_repl_basic.node_port
}

output "eq_sentinel_basic_port" {
  value = module.eq_sentinel_basic.node_port
}

output "eq_cluster_basic_port" {
  value = module.eq_cluster_basic.node_port
}

output "eq_key_header_port" {
  value = module.eq_key_header.node_port
}

output "eq_key_composite_port" {
  value = module.eq_key_composite.node_port
}

output "eq_burst_test_port" {
  value = module.eq_burst_test.node_port
}

output "eq_no_limit_port" {
  value = module.eq_no_limit.node_port
}

output "eq_protocol_port" {
  value = module.eq_protocol.node_port
}

output "eq_protocol_rl_port" {
  value = module.eq_protocol_rl.node_port
}
