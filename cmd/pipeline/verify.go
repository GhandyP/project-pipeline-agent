package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GhandyP/project-pipeline-agent/internal/tracker"
)

// runVerify reports the configuration and proves the data directory is usable
// without contacting Google or starting a server.
func runVerify(args []string, out io.Writer) error {
	flags := flagSet("verify", out)
	dataDir := flags.String("data", "", "directory holding the tracker (default PIPELINE_DATA_DIR)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *dataDir != "" {
		cfg.dataDir = *dataDir
	}

	var missing []string
	var problems []string
	report := func(name, status, note string) {
		fmt.Fprintf(out, "%-30s %-22s %s\n", name, status, note)
	}

	keyStatus := "set"
	switch {
	case strings.TrimSpace(cfg.apiKey) != "":
	case cfg.scripted:
		keyStatus = "not needed"
	default:
		keyStatus = "MISSING (required)"
		missing = append(missing, "GOOGLE_API_KEY")
	}
	report("GOOGLE_API_KEY", keyStatus, "Gemini key for the live agent")
	report("ADK_MODEL", cfg.model, "model the live agent runs on")
	report("PIPELINE_SCRIPTED_MODEL", fmt.Sprintf("%t", cfg.scripted), "deterministic offline model")

	if store, err := tracker.New(cfg.dataDir, systemClock{}); err != nil {
		report("PIPELINE_DATA_DIR", "UNUSABLE", err.Error())
		problems = append(problems, "PIPELINE_DATA_DIR")
	} else {
		report("PIPELINE_DATA_DIR", "writable", store.Dir())
		summaries, err := store.ListProjects(context.Background())
		if err != nil {
			report("projects", "UNREADABLE", err.Error())
			problems = append(problems, "projects")
		} else {
			report("projects", fmt.Sprintf("%d found", len(summaries)), "projects in the store")
		}
	}

	if entries, err := os.ReadDir(cfg.fixtures); err != nil {
		report("PIPELINE_FIXTURES", "not found", cfg.fixtures+" (only needed to seed a store)")
	} else {
		report("PIPELINE_FIXTURES", fmt.Sprintf("%d file(s)", len(entries)), cfg.fixtures)
	}

	overdueDays := int(cfg.policy.CriticalOverdue.Hours() / 24)
	stalledDays := int(cfg.policy.StalledAfter.Hours() / 24)
	report("PIPELINE_CRITICAL_OVERDUE_DAYS", fmt.Sprintf("%d", overdueDays), "days late that make a milestone critical")
	report("PIPELINE_STALLED_AFTER_DAYS", fmt.Sprintf("%d", stalledDays), "days without an update before stalled")
	report("PIPELINE_ADDR", cfg.addr, "address the web app listens on")

	if cfg.policy.StalledAfter <= 0 {
		problems = append(problems, "PIPELINE_STALLED_AFTER_DAYS")
	}
	if cfg.policy.CriticalOverdue <= 0 {
		problems = append(problems, "PIPELINE_CRITICAL_OVERDUE_DAYS")
	}

	switch {
	case len(missing) > 0:
		return fmt.Errorf("missing required configuration: %s (use the offline model with PIPELINE_SCRIPTED_MODEL=true)", strings.Join(missing, ", "))
	case len(problems) > 0:
		return fmt.Errorf("configuration problems: %s", strings.Join(problems, ", "))
	}
	fmt.Fprintln(out, "\nAll required configuration is present. Nothing was contacted.")
	return nil
}
