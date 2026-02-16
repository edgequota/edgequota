package main

import (
	"fmt"
	"path/filepath"
	"time"
)

// terraformApply runs terraform init + apply for all infrastructure.
func terraformApply() {
	tfDir := filepath.Join(getE2EDir(), "terraform")

	info("Initializing Terraform...")

	if _, err := runInDir(tfDir, "terraform", "init"); err != nil {
		fatal("Terraform init failed: %v", err)
	}

	info("Applying Terraform configuration (Redis + EdgeQuota instances)...")

	if err := streamInDir(tfDir, "terraform", "apply",
		"-auto-approve",
		"-var", "namespace="+namespace,
		"-var", "edgequota_image="+imageName,
		"-var", "testbackend_image="+testbackendImageName,
		"-var", "mockextrl_image="+mockextrlImageName,
	); err != nil {
		fatal("Terraform apply failed: %v", err)
	}

	info("Waiting for infrastructure to be ready...")
	waitForInfrastructure()

	info("All infrastructure deployed and ready")
}

// terraformDestroy runs terraform destroy.
func terraformDestroy() {
	tfDir := filepath.Join(getE2EDir(), "terraform")

	info("Destroying Terraform infrastructure...")

	if err := streamInDir(tfDir, "terraform", "destroy",
		"-auto-approve",
		"-var", "namespace="+namespace,
		"-var", "edgequota_image="+imageName,
		"-var", "testbackend_image="+testbackendImageName,
		"-var", "mockextrl_image="+mockextrlImageName,
	); err != nil {
		warn("Terraform destroy encountered errors: %v", err)
	}
}

// waitForInfrastructure waits for all deployed pods to be ready.
func waitForInfrastructure() {
	checks := []struct {
		name     string
		selector string
	}{
		{"Redis Single", "app=redis-single"},
		{"Redis Primary (replication)", "app=redis-replication,role=master"},
		{"Redis Replicas (replication)", "app=redis-replication,role=replica"},
		{"Redis Primary (sentinel)", "app=redis-sentinel,role=master"},
		{"Redis Sentinels", "app=redis-sentinel-node"},
		{"Redis Cluster", "app=redis-cluster"},
		{"Whoami backend", "app=whoami"},
		{"Test backend (multi-protocol)", "app=testbackend"},
		{"EdgeQuota (single-pt)", "edgequota-scenario=single-pt"},
		{"EdgeQuota (single-fc)", "edgequota-scenario=single-fc"},
		{"EdgeQuota (single-fb)", "edgequota-scenario=single-fb"},
		{"EdgeQuota (repl-basic)", "edgequota-scenario=repl-basic"},
		{"EdgeQuota (sentinel-basic)", "edgequota-scenario=sentinel-basic"},
		{"EdgeQuota (cluster-basic)", "edgequota-scenario=cluster-basic"},
		{"EdgeQuota (key-header)", "edgequota-scenario=key-header"},
		{"EdgeQuota (key-composite)", "edgequota-scenario=key-composite"},
		{"EdgeQuota (burst-test)", "edgequota-scenario=burst-test"},
		{"EdgeQuota (no-limit)", "edgequota-scenario=no-limit"},
		{"EdgeQuota (protocol)", "edgequota-scenario=protocol"},
		{"EdgeQuota (protocol-rl)", "edgequota-scenario=protocol-rl"},
		{"EdgeQuota (protocol-h3)", "edgequota-scenario=protocol-h3"},
		{"Mock external RL service", "app=mockextrl"},
		{"EdgeQuota (dynamic-backend)", "edgequota-scenario=dynamic-backend"},
	}

	for _, c := range checks {
		info("Waiting for %s...", c.name)

		if err := waitForPods(c.selector, 3*time.Minute); err != nil {
			fatal("%s not ready: %v", c.name, err)
		}

		fmt.Printf("  ✓ %s ready\n", c.name)
	}

	// Wait for Redis Cluster init job.
	info("Waiting for Redis Cluster initialization job...")

	if err := waitForJob("redis-cluster-init", 2*time.Minute); err != nil {
		fatal("Redis Cluster init failed: %v", err)
	}

	fmt.Println("  ✓ Redis Cluster initialized")
}
