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
	case "serve", "run":
		if err := serve(cmd == "run"); err != nil {
			fmt.Fprintln(os.Stderr, "pullpilot:", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`pullpilot — secure, compose-aware container auto-updater

Usage:
  pullpilot serve     Run the daemon (schedule + optional webhook)   [default]
  pullpilot run       Run a single update cycle and exit
  pullpilot version   Print version

Configuration is via PP_* environment variables (see README).`)
}

func serve(once bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel, cfg.LogJSON)
	cfg.LogSummary(log)

	// Warn loudly if the data dir won't survive a restart.
	if persistent, known := config.DataDirPersistent(cfg.DataDir); known && !persistent {
		log.Warn().Str("data_dir", cfg.DataDir).
			Msg("data dir is NOT a persistent mount — on restart the webhook identity will change " +
				"(breaking your poke URL) and soak timers will reset. Mount a volume at PP_DATA_DIR.")
	}

	st, err := state.Load(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	notifier := notify.New(cfg.NotifyURL, log)
	eng, err := engine.New(cfg, st, log, notifier)
	if err != nil {
		return err
	}
	defer eng.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Single-flight guard so schedule and webhook never run concurrently.
	var mu sync.Mutex
	runCycle := func(trigger string) {
		mu.Lock()
		defer mu.Unlock()
		eng.Run(ctx, trigger)
	}

	if once {
		runCycle("manual")
		return nil
	}

	// Webhook listener (optional).
	if cfg.Webhook {
		wc, err := webhook.New(cfg.WebhookURL, cfg.DataDir, log, func(reason string) {
			runCycle("webhook")
		})
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

	log.Info().Str("schedule", cfg.Schedule).Str("tz", cfg.Timezone).Msg("daemon ready")
	<-ctx.Done()
	log.Info().Msg("shutting down")
	return nil
}
