package main

import (
	"strings"
)

// ensureMinikube starts a minikube cluster if one is not already running.
// Uses the QEMU driver with socket_vmnet so the VM gets a routable IP on the
// host network. This lets us reach NodePort services (TCP and UDP) directly
// from the host without kubectl port-forward.
func ensureMinikube() {
	info("Checking minikube status...")

	out, err := run("minikube", "status", "--format={{.Host}}")
	if err == nil && strings.TrimSpace(out) == "Running" {
		info("Minikube already running, reusing existing cluster")
		return
	}

	info("Starting minikube cluster (qemu2 + socket_vmnet)...")

	if err := runStream("minikube", "start",
		"--cpus=4",
		"--memory=8192",
		"--driver=qemu2",
		"--network=socket_vmnet",
		"--kubernetes-version=v1.34.0",
		"--addons=default-storageclass,storage-provisioner",
	); err != nil {
		fatal("Failed to start minikube: %v", err)
	}

	info("Minikube cluster started")
}

// getMinikubeIP returns the routable IP address of the minikube VM.
func getMinikubeIP() string {
	out, err := run("minikube", "ip")
	if err != nil {
		fatal("Cannot get minikube IP: %v", err)
	}

	ip := strings.TrimSpace(out)
	if ip == "" {
		fatal("minikube ip returned empty string")
	}

	return ip
}

// destroyMinikube deletes the minikube cluster.
func destroyMinikube() {
	info("Deleting minikube cluster...")

	if err := runStream("minikube", "delete"); err != nil {
		warn("Failed to delete minikube (may already be gone): %v", err)
	}
}
