package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Report types
// ---------------------------------------------------------------------------

// Report is the top-level test run report written to disk after each e2e run.
type Report struct {
	Timestamp  time.Time     `json:"timestamp"`
	Duration   time.Duration `json:"duration"`
	MinikubeIP string        `json:"minikube_ip"`
	PassCount  int           `json:"pass_count"`
	FailCount  int           `json:"fail_count"`
	TotalCount int           `json:"total_count"`
	AllPassed  bool          `json:"all_passed"`
	Tests      []TestEntry   `json:"tests"`
	PodLogs    []PodLog      `json:"pod_logs,omitempty"`
}

// TestEntry records the outcome of a single test case.
type TestEntry struct {
	Index    int           `json:"index"`
	Name     string        `json:"name"`
	TestID   string        `json:"test_id"`
	Passed   bool          `json:"passed"`
	Detail   string        `json:"detail"`
	Duration time.Duration `json:"duration_ns"`
	// DurationHuman is a human-readable duration string (not serialized, used for markdown).
	DurationHuman string `json:"-"`
}

// PodLog stores logs captured from a Kubernetes pod.
type PodLog struct {
	Pod  string `json:"pod"`
	Logs string `json:"logs"`
}

// ---------------------------------------------------------------------------
// Log collection
// ---------------------------------------------------------------------------

// collectEdgeQuotaLogs fetches the last 200 log lines from every EdgeQuota pod
// in the e2e namespace. It only collects logs when there are failures.
func collectEdgeQuotaLogs() []PodLog {
	// List all EdgeQuota pods (they have an "edgequota-scenario" label).
	out, err := kubectl("get", "pods", "-n", namespace,
		"-l", "edgequota-scenario",
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}

	podNames := strings.Fields(strings.TrimSpace(out))
	logs := make([]PodLog, 0, len(podNames))

	for _, pod := range podNames {
		logOut, logErr := kubectl("logs", "-n", namespace, pod, "--tail=200")
		if logErr != nil {
			logOut = fmt.Sprintf("(failed to collect logs: %v)", logErr)
		}

		logs = append(logs, PodLog{
			Pod:  pod,
			Logs: strings.TrimSpace(logOut),
		})
	}

	return logs
}

// ---------------------------------------------------------------------------
// Report writing
// ---------------------------------------------------------------------------

const reportsDir = "reports"

// writeReport writes both a JSON and a Markdown report file for the test run.
func writeReport(r *Report) string {
	if err := os.MkdirAll(reportsDir, 0o755); err != nil {
		warn("could not create reports directory: %v", err)
		return ""
	}

	ts := r.Timestamp.Format("2006-01-02T15-04-05")
	baseName := fmt.Sprintf("%s/%s", reportsDir, ts)

	writeJSON(baseName+".json", r)
	writeMarkdown(baseName+".md", r)

	return baseName + ".md"
}

func writeJSON(path string, r *Report) {
	f, err := os.Create(path)
	if err != nil {
		warn("could not create report %s: %v", path, err)
		return
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")

	if err := enc.Encode(r); err != nil {
		warn("could not write report %s: %v", path, err)
	}
}

func writeMarkdown(path string, r *Report) {
	var b strings.Builder

	// Header.
	b.WriteString("# EdgeQuota E2E Test Report\n\n")
	fmt.Fprintf(&b, "**Date:** %s  \n", r.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(&b, "**Duration:** %s  \n", r.Duration.Round(time.Millisecond))
	fmt.Fprintf(&b, "**Minikube IP:** %s  \n", r.MinikubeIP)
	fmt.Fprintf(&b, "**Result:** %d passed, %d failed, %d total  \n\n", r.PassCount, r.FailCount, r.TotalCount)

	if r.AllPassed {
		b.WriteString("## All tests passed\n\n")
	} else {
		b.WriteString("## FAILURES DETECTED\n\n")
	}

	// Results table.
	b.WriteString("## Test Results\n\n")
	b.WriteString("| # | Status | Test | Duration | Detail |\n")
	b.WriteString("|---|--------|------|----------|--------|\n")

	for _, t := range r.Tests {
		status := "PASS"
		if !t.Passed {
			status = "**FAIL**"
		}

		// Escape pipes in detail for Markdown table.
		detail := strings.ReplaceAll(t.Detail, "|", "\\|")
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n",
			t.Index, status, t.Name, t.DurationHuman, detail)
	}

	b.WriteString("\n")

	// Failure details section.
	failures := make([]TestEntry, 0)
	for _, t := range r.Tests {
		if !t.Passed {
			failures = append(failures, t)
		}
	}

	if len(failures) > 0 {
		b.WriteString("## Failed Tests\n\n")

		for _, t := range failures {
			fmt.Fprintf(&b, "### %d. %s (`%s`)\n\n", t.Index, t.Name, t.TestID)
			fmt.Fprintf(&b, "**Duration:** %s  \n", t.DurationHuman)
			fmt.Fprintf(&b, "**Detail:** %s\n\n", t.Detail)
		}
	}

	// Pod logs section (only when there are failures).
	if len(r.PodLogs) > 0 {
		b.WriteString("## EdgeQuota Pod Logs\n\n")
		b.WriteString("Logs from all EdgeQuota pods at the time of test completion.\n\n")

		for _, pl := range r.PodLogs {
			fmt.Fprintf(&b, "### `%s`\n\n", pl.Pod)
			fmt.Fprintf(&b, "```\n")
			// Truncate very long logs to keep the report manageable.
			logs := pl.Logs
			if len(logs) > 10000 {
				logs = logs[len(logs)-10000:]
				b.WriteString("... (truncated, showing last 10000 chars)\n")
			}
			b.WriteString(logs)
			b.WriteString("\n```\n\n")
		}
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		warn("could not write markdown report %s: %v", path, err)
		return
	}

	absPath, _ := filepath.Abs(path)
	info("Report written to %s", absPath)
}
