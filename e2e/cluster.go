package main

import (
	"os"
	"path/filepath"
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

	args := []string{
		"start",
		"--cpus=4",
		"--memory=8192",
		"--driver=qemu2",
		"--network=socket_vmnet",
		"--kubernetes-version=v1.34.0",
		"--addons=default-storageclass,storage-provisioner",
	}
	args = append(args, socketVmnetFlags()...)

	if err := runStream("minikube", args...); err != nil {
		fatal("Failed to start minikube: %v", err)
	}

	info("Minikube cluster started")
}

// socketVmnetFlags points minikube at the socket_vmnet socket and client binary.
// minikube defaults to /var/run/socket_vmnet and
// /opt/socket_vmnet/bin/socket_vmnet_client, but Homebrew installs them under
// its own prefix (/opt/homebrew on Apple Silicon, /usr/local on Intel), so
// without these flags minikube cannot find a Homebrew-installed socket_vmnet and
// the cluster never starts. Paths may be overridden with
// EDGEQUOTA_SOCKET_VMNET_PATH / EDGEQUOTA_SOCKET_VMNET_CLIENT_PATH; otherwise
// they are discovered via brew, and any path that does not resolve is left to
// minikube's default.
func socketVmnetFlags() []string {
	socket := firstExisting(
		os.Getenv("EDGEQUOTA_SOCKET_VMNET_PATH"),
		brewPath("", "var/run/socket_vmnet"),
	)
	client := firstExisting(
		os.Getenv("EDGEQUOTA_SOCKET_VMNET_CLIENT_PATH"),
		brewPath("socket_vmnet", "bin/socket_vmnet_client"),
	)

	var flags []string
	if socket != "" {
		flags = append(flags, "--socket-vmnet-path="+socket)
	}
	if client != "" {
		flags = append(flags, "--socket-vmnet-client-path="+client)
	}
	return flags
}

// brewPath joins rel onto a Homebrew prefix. With no formula it uses the
// top-level prefix (where var/run lives); with a formula it uses that formula's
// own prefix (where the versioned binary lives). Returns "" if brew is absent.
func brewPath(formula, rel string) string {
	brewArgs := []string{"--prefix"}
	if formula != "" {
		brewArgs = append(brewArgs, formula)
	}
	out, err := run("brew", brewArgs...)
	if err != nil {
		return ""
	}
	prefix := strings.TrimSpace(out)
	if prefix == "" {
		return ""
	}
	return filepath.Join(prefix, rel)
}

// firstExisting returns the first candidate that exists on disk, or "".
func firstExisting(candidates ...string) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
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
