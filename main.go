package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run implements the whole of Phase 0's behavior: load the config, print it
// redacted, and exit. It takes stdout/stderr explicitly so it is testable
// without touching the real process streams.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cutoffarr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/config/config.yml", "path to the cutoffarr YAML config file")
	once := fs.Bool("once", false, "run a single pass and exit (daemon mode arrives in a later phase)")
	forceDryRun := fs.Bool("dry-run", false, "force dry-run mode on; cannot be used to disable dry-run set by config")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "cutoffarr: fatal: %v\n", err)
		return 1
	}

	// --dry-run is a force-on override only: it can make dry-run true but
	// never turns it off. Config governs otherwise.
	if *forceDryRun {
		cfg.DryRun = true
	}

	logger := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slogLevel(cfg.LogLevel)}))

	if len(cfg.Instances) == 0 {
		logger.Warn("no instances configured")
	}

	logConfig(logger, *cfg)

	if !*once {
		logger.Info("daemon mode is not implemented yet; it arrives in a later phase")
	}

	return 0
}

// slogLevel maps a validated log_level string to its slog.Level. Config
// validation guarantees one of these four values by the time this is
// called; the default case only exists to satisfy the compiler.
func slogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// logConfig prints the loaded configuration with every API key redacted.
// Instances are logged individually so the api_key placeholder is always
// explicit in the output, never merely omitted.
func logConfig(logger *slog.Logger, cfg Config) {
	redacted := cfg.Redacted()

	logger.Info("configuration loaded",
		"dry_run", redacted.DryRun,
		"poll_interval", redacted.PollInterval.String(),
		"webhook_port", redacted.WebhookPort,
		"webhook_debounce", redacted.WebhookDebounce.String(),
		"log_level", redacted.LogLevel,
		"exclusion_tag", redacted.ExclusionTag,
		"instance_count", len(redacted.Instances),
	)

	for _, inst := range redacted.Instances {
		logger.Info("configured instance",
			"name", inst.Name,
			"type", inst.Type,
			"url", inst.URL,
			"api_key", inst.APIKey,
		)
	}
}
