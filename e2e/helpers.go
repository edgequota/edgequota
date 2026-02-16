package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	namespace            = "edgequota-e2e"
	imageName            = "edgequota:e2e"
	testbackendImageName = "testbackend:e2e"
	mockextrlImageName   = "mockextrl:e2e"
)

// run executes a command, printing it to stdout, and returns combined output.
func run(name string, args ...string) (string, error) {
	fmt.Printf("  $ %s %s\n", name, strings.Join(args, " "))

	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	output := buf.String()

	if err != nil {
		return output, fmt.Errorf("%s %s failed: %w\n%s", name, strings.Join(args, " "), err, output)
	}

	return output, nil
}

// runStream executes a command, streaming output to stdout/stderr in real time.
func runStream(name string, args ...string) error {
	fmt.Printf("  $ %s %s\n", name, strings.Join(args, " "))

	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// runInDir executes a command in a specific directory.
func runInDir(dir, name string, args ...string) (string, error) {
	fmt.Printf("  [%s] $ %s %s\n", dir, name, strings.Join(args, " "))

	cmd := exec.Command(name, args...)
	cmd.Dir = dir

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	output := buf.String()

	if err != nil {
		return output, fmt.Errorf("%s failed in %s: %w\n%s", name, dir, err, output)
	}

	return output, nil
}

// streamInDir runs a command in a directory with streaming output.
func streamInDir(dir, name string, args ...string) error {
	fmt.Printf("  [%s] $ %s %s\n", dir, name, strings.Join(args, " "))

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// kubectl runs a kubectl command.
func kubectl(args ...string) (string, error) {
	return run("kubectl", args...)
}

// waitForPods waits until all pods matching a label selector are ready.
func waitForPods(selector string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		out, err := kubectl("get", "pods", "-n", namespace,
			"-l", selector,
			"-o", "jsonpath={.items[*].status.conditions[?(@.type=='Ready')].status}")
		if err == nil && out != "" {
			statuses := strings.Fields(out)
			allReady := len(statuses) > 0

			for _, s := range statuses {
				if s != "True" {
					allReady = false
					break
				}
			}

			if allReady {
				return nil
			}
		}

		time.Sleep(3 * time.Second)
	}

	// Print pod status for debugging.
	out, _ := kubectl("get", "pods", "-n", namespace, "-l", selector, "-o", "wide")
	return fmt.Errorf("timeout waiting for pods (selector=%s):\n%s", selector, out)
}

// waitForJob waits until a job completes.
func waitForJob(jobName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		out, err := kubectl("get", "job", jobName, "-n", namespace,
			"-o", "jsonpath={.status.succeeded}")
		if err == nil && strings.TrimSpace(out) == "1" {
			return nil
		}

		// Check for failure.
		failOut, _ := kubectl("get", "job", jobName, "-n", namespace,
			"-o", "jsonpath={.status.failed}")
		if strings.TrimSpace(failOut) != "" && strings.TrimSpace(failOut) != "0" {
			logs, _ := kubectl("logs", "-n", namespace, fmt.Sprintf("job/%s", jobName), "--tail=50")
			return fmt.Errorf("job %s failed:\n%s", jobName, logs)
		}

		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("timeout waiting for job %s", jobName)
}

// deletePod deletes a pod by label selector (first match).
func deletePod(selector string) error {
	out, err := kubectl("get", "pods", "-n", namespace,
		"-l", selector,
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return fmt.Errorf("finding pod: %w", err)
	}

	podName := strings.TrimSpace(out)
	if podName == "" {
		return fmt.Errorf("no pod found for selector %s", selector)
	}

	_, err = kubectl("delete", "pod", podName, "-n", namespace, "--grace-period=0", "--force")

	return err
}

// banner prints a section header.
func banner(msg string) {
	fmt.Printf("\n%s\n", strings.Repeat("=", 70))
	fmt.Printf("  %s\n", msg)
	fmt.Printf("%s\n\n", strings.Repeat("=", 70))
}

// info prints an info line.
func info(format string, args ...any) {
	fmt.Printf("[INFO] "+format+"\n", args...)
}

// warn prints a warning line.
func warn(format string, args ...any) {
	fmt.Printf("[WARN] "+format+"\n", args...)
}

// fatal prints an error and exits.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[FATAL] "+format+"\n", args...)
	os.Exit(1)
}

// fileExists checks whether a path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pollUntil calls check repeatedly with exponential backoff until it returns
// true or the timeout expires. Initial interval is 1s, max interval is 10s.
// Returns an error if the timeout is exceeded.
func pollUntil(timeout time.Duration, desc string, check func() bool) error {
	deadline := time.Now().Add(timeout)
	interval := time.Second

	for time.Now().Before(deadline) {
		if check() {
			return nil
		}
		sleep := interval
		if remaining := time.Until(deadline); sleep > remaining {
			sleep = remaining
		}
		time.Sleep(sleep)
		interval = min(interval*2, 10*time.Second)
	}
	return fmt.Errorf("timeout after %s waiting for: %s", timeout, desc)
}

// waitForHTTPCondition polls an HTTP endpoint until the check function returns
// true or the timeout expires.
func waitForHTTPCondition(base string, timeout time.Duration, desc string, check func(ok200, ok429 int) bool) error {
	return pollUntil(timeout, desc, func() bool {
		ok200, ok429 := sendBurst(base, nil, 5)
		return check(ok200, ok429)
	})
}
