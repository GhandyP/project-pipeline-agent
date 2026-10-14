package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GhandyP/project-pipeline-agent/internal/agent"
	"github.com/GhandyP/project-pipeline-agent/internal/service"
	"github.com/GhandyP/project-pipeline-agent/internal/web"
)

// runServe starts the web app.
//
// The read timeout is short, the write timeout is left unset because the review
// stream is long by nature, and the stream itself is bounded by the request
// context, so closing the tab stops the run.
func runServe(args []string, out io.Writer) error {
	flags := flagSet("serve", out)
	addr := flags.String("addr", "", "address to listen on (default PIPELINE_ADDR or "+defaultAddr+")")
	dataDir := flags.String("data", "", "directory holding the tracker (default PIPELINE_DATA_DIR or "+defaultDataDir+")")
	seed := flags.String("seed", "", "seed the store from a fixture directory when it is empty (default "+defaultFixtures+")")
	scripted := flags.Bool("scripted", false, "use the deterministic scripted model instead of Gemini")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.addr = *addr
	}
	if *dataDir != "" {
		cfg.dataDir = *dataDir
	}
	seedDir := cfg.fixtures
	if *seed != "" {
		seedDir = *seed
	}

	ctx := context.Background()
	llm, err := cfg.llm(ctx, *scripted)
	if err != nil {
		return err
	}

	_, svc, err := openStore(cfg, seedDir, out)
	if err != nil {
		return err
	}
	reviewer, err := agent.NewReviewer(llm, svc)
	if err != nil {
		return err
	}
	server, err := web.New(web.Config{
		Service:  svc,
		Streamer: reviewer,
		Policy:   cfg.policy,
		Clock:    systemClock{},
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(out, "Pipeline review on http://%s\n", cfg.addr)
	fmt.Fprintf(out, "Tracker data: %s\n", cfg.dataDir)
	if *scripted || cfg.scripted {
		fmt.Fprintln(out, "Model: scripted (deterministic, no credential)")
	} else {
		fmt.Fprintf(out, "Model: %s\n", cfg.model)
	}
	fmt.Fprintln(out, "Nothing is written until a named human approves an update.")

	failure := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failure <- err
			return
		}
		failure <- nil
	}()

	select {
	case err := <-failure:
		return err
	case <-stop.Done():
		shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		fmt.Fprintln(out, "\nStopping.")
		return httpServer.Shutdown(shutdown)
	}
}

// Compile-time proof that the wiring is the wiring: the service satisfies what
// the agent's tools need and what the web app asks for.
var (
	_ agent.Actions = (*service.Service)(nil)
	_ web.Service   = (*service.Service)(nil)
)
