// Command pipeline reviews a project pipeline and, only with a named human
// approval, applies the update the review proposed.
//
// The subcommands are the web app, the terminal review, the offline demo, a
// configuration check, and the ADK's own console and web launcher over the same
// agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"

	"github.com/GhandyP/project-pipeline-agent/internal/agent"
	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/service"
	"github.com/GhandyP/project-pipeline-agent/internal/tracker"
)

const (
	defaultModel    = "gemini-flash-latest"
	defaultDataDir  = "./data"
	defaultAddr     = "127.0.0.1:8080"
	defaultFixtures = "testdata/projects"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pipeline: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		usage(stdout)
		return nil
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout)
	case "review":
		return runReview(args[1:], stdout)
	case "demo":
		return runDemo(args[1:], stdin, stdout)
	case "verify":
		return runVerify(args[1:], stdout)
	case "adk":
		return runADK(args[1:], stdout)
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stdout)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage(out io.Writer) {
	fmt.Fprint(out, `pipeline — review a project pipeline, then decide

Usage:
  pipeline serve    [--addr :8080] [--data ./data] [--seed testdata/projects] [--scripted]
  pipeline review   --project <id> [--data ./data] [--scripted]
  pipeline demo     [--project <id>] [--fixtures testdata/projects] [--as <name>] [--approve]
  pipeline verify   [--data ./data]
  pipeline adk      console | web | api webui | ...   (the ADK's own launcher)

The rules decide what is a blocker: severity, owner state and evidence are pure
Go. The agent explains and prioritizes them and can propose a dry-run update,
but no tool of its own can write. A tracker change happens only after a named
human approves it, and the tracker re-checks the project before it writes.

The offline path needs no credential:
  pipeline demo --project delta-onboarding --approve
`)
}

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------

type config struct {
	dataDir  string
	addr     string
	apiKey   string
	model    string
	fixtures string
	scripted bool
	policy   domain.Policy
}

// systemClock is the only clock the binary uses outside tests.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func loadConfig() (config, error) {
	cfg := config{
		dataDir:  envOr("PIPELINE_DATA_DIR", defaultDataDir),
		addr:     envOr("PIPELINE_ADDR", defaultAddr),
		apiKey:   strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")),
		model:    envOr("ADK_MODEL", defaultModel),
		fixtures: envOr("PIPELINE_FIXTURES", defaultFixtures),
		policy:   domain.DefaultPolicy(),
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("PIPELINE_SCRIPTED_MODEL"))) {
	case "1", "true", "yes", "on":
		cfg.scripted = true
	}

	overdueDays, err := envDays("PIPELINE_CRITICAL_OVERDUE_DAYS", 30)
	if err != nil {
		return cfg, err
	}
	stalledDays, err := envDays("PIPELINE_STALLED_AFTER_DAYS", 14)
	if err != nil {
		return cfg, err
	}
	cfg.policy.CriticalOverdue = time.Duration(overdueDays) * 24 * time.Hour
	cfg.policy.StalledAfter = time.Duration(stalledDays) * 24 * time.Hour
	return cfg, nil
}

// envDays reads a whole number of days, refusing a negative or unparsable
// value instead of silently keeping the default.
func envDays(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a whole number of days", name, raw)
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return value, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// llm builds the model the agent runs on: the deterministic scripted model, or
// Gemini when a key is present.
func (c config) llm(ctx context.Context, scripted bool) (model.LLM, error) {
	if scripted || c.scripted {
		return agent.NewScriptedModel(), nil
	}
	if c.apiKey == "" {
		return nil, errors.New("GOOGLE_API_KEY is required for the live agent; use --scripted or PIPELINE_SCRIPTED_MODEL=true for the offline model")
	}
	built, err := gemini.NewModel(ctx, c.model, &genai.ClientConfig{APIKey: c.apiKey})
	if err != nil {
		return nil, fmt.Errorf("build the Gemini model: %w", err)
	}
	return built, nil
}

// openStore opens the tracker and, when asked, seeds it from a fixture
// directory. Seeding never overwrites a project that is already there.
func openStore(cfg config, seedDir string, out io.Writer) (*tracker.Store, *service.Service, error) {
	store, err := tracker.New(cfg.dataDir, systemClock{})
	if err != nil {
		return nil, nil, err
	}
	if seedDir != "" {
		if summaries, err := store.ListProjects(context.Background()); err == nil && len(summaries) == 0 {
			seeded, err := store.Seed(seedDir)
			if err != nil {
				return nil, nil, err
			}
			if seeded > 0 {
				fmt.Fprintf(out, "Seeded %d project(s) from %s into %s\n", seeded, seedDir, store.Dir())
			}
		}
	}
	svc, err := service.New(store, systemClock{}, cfg.policy)
	if err != nil {
		return nil, nil, err
	}
	return store, svc, nil
}

// flagSet builds a flag set that reports errors to out instead of the process
// stderr, so the binary stays testable.
func flagSet(name string, out io.Writer) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(out)
	return set
}
