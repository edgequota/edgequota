// Package main is the orchestrator for end-to-end testing of EdgeQuota.
// It provisions a local Kubernetes cluster (minikube), deploys Redis
// topologies + EdgeQuota instances via Terraform, and runs a comprehensive
// test suite covering every topology, failure policy, and key-strategy
// combination with static rate-limit configuration.
//
// Usage:
//
//	go run ./e2e setup      — provision cluster + infrastructure
//	go run ./e2e test       — run E2E tests (cluster must be up)
//	go run ./e2e teardown   — destroy everything
//	go run ./e2e all        — setup → test → teardown
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "setup":
		doSetup()
	case "test":
		doTest()
	case "teardown":
		doTeardown()
	case "all":
		doSetup()
		ok := doTest()
		doTeardown()

		if !ok {
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`Usage: go run ./e2e <command>

Commands:
  setup      Provision minikube cluster and deploy all infrastructure via Terraform
  test       Run the full E2E test suite (cluster must already be up)
  teardown   Destroy minikube cluster and all resources
  all        setup → test → teardown (full cycle)`)
}

func doSetup() {
	banner("SETUP: Provisioning E2E environment")

	ensureMinikube()
	buildAndLoadImage()
	terraformApply()

	banner("SETUP COMPLETE")
	info("All Redis topologies + EdgeQuota instances deployed in namespace %q", namespace)
}

func doTest() bool {
	banner("RUNNING E2E TESTS")

	passed := runAllTests()

	if passed {
		banner("ALL TESTS PASSED")
	} else {
		banner("SOME TESTS FAILED")
	}

	return passed
}

func doTeardown() {
	banner("TEARDOWN")

	terraformDestroy()
	destroyMinikube()

	banner("TEARDOWN COMPLETE")
}
