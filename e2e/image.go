package main

import (
	"os"
	"path/filepath"
)

// buildAndLoadImage builds both the EdgeQuota and testbackend Docker images
// and loads them into minikube's Docker daemon.
func buildAndLoadImage() {
	projectDir := getProjectDir()

	// Build EdgeQuota image.
	info("Building EdgeQuota Docker image...")

	if err := runStream("docker", "build",
		"-t", imageName,
		"-f", filepath.Join(projectDir, "Dockerfile"),
		"--build-arg", "VERSION=e2e",
		projectDir,
	); err != nil {
		fatal("Docker build (edgequota) failed: %v", err)
	}

	// Build multi-protocol test backend image.
	info("Building testbackend Docker image...")

	testbackendDir := filepath.Join(getE2EDir(), "testbackend")
	if err := runStream("docker", "build",
		"-t", testbackendImageName,
		"-f", filepath.Join(testbackendDir, "Dockerfile"),
		testbackendDir,
	); err != nil {
		fatal("Docker build (testbackend) failed: %v", err)
	}

	// Load both into minikube.
	info("Loading images into minikube...")

	for _, img := range []string{imageName, testbackendImageName} {
		if err := runStream("minikube", "image", "load", img); err != nil {
			fatal("Failed to load %s image into minikube: %v", img, err)
		}
	}

	info("Images loaded: %s, %s", imageName, testbackendImageName)
}

// getProjectDir returns the absolute path to the project root.
func getProjectDir() string {
	wd, err := os.Getwd()
	if err != nil {
		fatal("Cannot get working directory: %v", err)
	}

	// If we're in the e2e directory.
	if filepath.Base(wd) == "e2e" {
		return filepath.Dir(wd)
	}

	// If we're in the project root.
	if fileExists(filepath.Join(wd, "cmd", "edgequota")) {
		return wd
	}

	fatal("Cannot locate project root from %s", wd)
	return ""
}

// getE2EDir returns the absolute path to the e2e/ directory.
func getE2EDir() string {
	wd, err := os.Getwd()
	if err != nil {
		fatal("Cannot get working directory: %v", err)
	}

	if filepath.Base(wd) == "e2e" {
		return wd
	}

	candidate := filepath.Join(wd, "e2e")
	if fileExists(candidate) {
		return candidate
	}

	fatal("Cannot locate e2e directory from %s", wd)
	return ""
}
