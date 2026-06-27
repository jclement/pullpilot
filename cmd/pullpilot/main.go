// Command pullpilot is a secure, compose-aware container auto-updater.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	mrand "math/rand/v2"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"github.com/jclement/pullpilot/internal/config"
	"github.com/jclement/pullpilot/internal/engine"
	"github.com/jclement/pullpilot/internal/logging"
	"github.com/jclement/pullpilot/internal/notify"
	"github.com/jclement/pullpilot/internal/state"
	"github.com/jclement/pullpilot/internal/version"
	"github.com/jclement/pullpilot/internal/webhook"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "version", "-v", "--version":
		fmt.Println("pullpilot", version.String())
	case "serve":
		exitOnErr(serve())
	case "run":
		exitOnErr(runOnce())
	case "status":
		exitOnErr(showStatus())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		usage()
		os.Exit(2)
	}
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "pullpilot:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`pullpilot — secure, compose-aware container auto-updater

Usage:
  pullpilot serve     Run the daemon (schedule + optional webhook)   [default]
  pullpilot status    Show each managed container and what would happen (read-only)
  pullpilot run       Run a single update cycle now and exit (honors PP_DRY_RUN)
  pullpilot version   Print version

Configuration is via PP_* environment variables (see README).`)
}

// build wires config, logging, state and the engine. When boot is false (e.g.
// `status`) it stays quiet — just warnings and the command's own output.
func build(boot bool) (*config.Config, zerolog.Logger, *engine.Engine, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, zerolog.Nop(), nil, err
	}
	level := cfg.LogLevel
	if !boot {
		level = "warn"
	}
	log := logging.New(level, cfg.LogJSON)
	if boot {
		cfg.LogSummary(log)
		if persistent, known := config.DataDirPersistent(cfg.DataDir); known && !persistent {
			log.Warn().Str("data_dir", cfg.DataDir).
				Msg("data dir is NOT a persistent mount — on restart the webhook identity will change " +
					"(breaking your poke URL) and soak timers will reset. Mount a volume at PP_DATA_DIR.")
		}
	}
	st, err := state.Load(cfg.DataDir)
	if err != nil {
		return cfg, log, nil, fmt.Errorf("load state: %w", err)
	}
	eng, err := engine.New(cfg, st, log, notify.New(cfg.NotifyURL, log))
	if err != nil {
		return cfg, log, nil, err
	}
	return cfg, log, eng, nil
}

// ping verifies Docker is reachable, bounded so a hung socket can't block boot.
func ping(ctx context.Context, eng *engine.Engine) error {
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return eng.Ping(pingCtx)
}

func showStatus() error {
	_, _, eng, err := build(false)
	if err != nil {
		return err
	}
	defer eng.Close()
	ctx := context.Background()
	if err := ping(ctx, eng); err != nil {
		return err
	}
	return eng.Status(ctx)
}

func runOnce() error {
	_, _, eng, err := build(true)
	if err != nil {
		return err
	}
	defer eng.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := ping(ctx, eng); err != nil {
		return err
	}
	eng.Run(ctx, "manual")
	return nil
}

func serve() error {
	cfg, log, eng, err := build(true)
	if err != nil {
		return err
	}
	defer eng.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pingOK := true
	if err := ping(ctx, eng); err != nil {
		pingOK = false
		log.Error().Err(err).Msg("Docker is not reachable — will keep retrying on schedule")
	}

	// Single-flight guard so schedule and webhook never run concurrently.
	var mu sync.Mutex
	runCycle := func(trigger string) {
		mu.Lock()
		defer mu.Unlock()
		eng.Run(ctx, trigger)
	}

	// Webhook listener (optional).
	if cfg.Webhook {
		wc, err := webhook.New(cfg.WebhookURL, cfg.DataDir, log, func(string) { runCycle("webhook") })
		if err != nil {
			log.Error().Err(err).Msg("webhook disabled (provisioning failed)")
		} else {
			go wc.Run(ctx)
		}
	}

	// Scheduler.
	c := cron.New(cron.WithLocation(cfg.Location()))
	_, err = c.AddFunc(cfg.Schedule, func() {
		if cfg.Jitter > 0 {
			d := time.Duration(mrand.Int64N(int64(cfg.Jitter)))
			log.Debug().Dur("jitter", d.Round(time.Second)).Msg("applying scheduled jitter")
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}
		runCycle("schedule")
	})
	if err != nil {
		return fmt.Errorf("invalid PP_SCHEDULE %q: %w", cfg.Schedule, err)
	}
	c.Start()
	defer c.Stop()

	// Run once at startup, then wait for schedule/webhook/signal.
	go runCycle("startup")

	if pingOK {
		log.Info().Str("schedule", cfg.Schedule).Str("tz", cfg.Timezone).Msg("daemon ready")
	} else {
		log.Warn().Msg("daemon started but Docker is unreachable — fix the socket mount/permissions (see error above)")
	}
	<-ctx.Done()
	log.Info().Msg("shutting down")

	// Give an in-flight update a moment to finish cleanly; any recreate that is
	// still interrupted is reconciled on the next start.
	done := make(chan struct{})
	go func() { mu.Lock(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		log.Warn().Msg("shutdown timed out waiting for an in-flight update")
	}
	return nil
}
