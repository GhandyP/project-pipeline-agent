package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/full"
	"google.golang.org/adk/v2/session"

	"github.com/GhandyP/project-pipeline-agent/internal/agent"
)

// runADK hands the agent to the ADK's own launcher, so the console, the web UI
// and the REST surface the framework ships are available over the same tools
// and the same service.
//
// This path uses the live model: the framework's UI has no use for a scripted
// answer.
func runADK(args []string, out io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	llm, err := cfg.llm(ctx, false)
	if err != nil {
		return err
	}
	_, svc, err := openStore(cfg, "", out)
	if err != nil {
		return err
	}
	reviewAgent, err := agent.New(llm, svc)
	if err != nil {
		return err
	}

	config := &launcher.Config{
		AgentLoader:    adkagent.NewSingleLoader(reviewAgent),
		SessionService: session.InMemoryService(),
	}
	app := full.NewLauncher()
	if err := app.Execute(ctx, config, args); err != nil {
		return fmt.Errorf("%w\n\n%s", err, strings.TrimSpace(app.CommandLineSyntax()))
	}
	return nil
}
