package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run loads the config, prints it redacted, and (in --once mode) checks
// connectivity against every configured instance before exiting. It takes
// stdout/stderr explicitly so it is testable without touching the real
// process streams.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cutoffarr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/config/config.yml", "path to the cutoffarr YAML config file")
	once := fs.Bool("once", false, "run a single pass and exit (daemon mode arrives in a later phase)")
	forceDryRun := fs.Bool("dry-run", false, "force dry-run mode on; cannot be used to disable dry-run set by config")
	samplesFlag := fs.String("samples", "", "comma-separated movie titles to dump full detail for during Radarr library inspection (--once only)")

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

	// The redacted config printout is a mandatory startup behavior, not a
	// leveled log message: it must appear regardless of log_level (e.g.
	// warn or error), so it bypasses the slog handler's level filter and
	// goes straight to stdout. It must also be the first thing printed, so
	// it runs before any other log line, including the no-instances
	// warning below.
	printRedactedConfig(stdout, *cfg)

	if len(cfg.Instances) == 0 {
		logger.Warn("no instances configured")
	}

	if *once {
		// Phase 1: for each configured instance, run the read-only
		// connectivity check sequentially. Any one instance's failure is
		// logged as a warning by checkInstanceConnectivity itself and
		// must not stop the remaining instances or affect the exit code.
		//
		// Phase 2: for radarr instances only, follow up with the read-only
		// library inspection (GET /movie, paged GET /wanted/cutoff).
		// Sonarr instances are connectivity-only until Phase 6.
		samples := parseSamples(*samplesFlag)
		for _, inst := range cfg.Instances {
			checkInstanceConnectivity(context.Background(), logger, inst)
			if inst.Type == "radarr" {
				inspectRadarrLibrary(context.Background(), logger, inst, samples)
			}
		}
	} else {
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

// printRedactedConfig prints the loaded configuration with every API key
// redacted, writing directly to w rather than through the leveled logger.
// This is deliberate: "print the redacted config at startup" is a mandatory
// behavior, not a leveled log message, so it must be visible regardless of
// the configured log_level (including warn and error, which would otherwise
// filter out an Info-level log record). Instances are printed individually
// so the api_key placeholder is always explicit in the output, never merely
// omitted.
func printRedactedConfig(w io.Writer, cfg Config) {
	redacted := cfg.Redacted()

	fmt.Fprintf(w, "configuration loaded: dry_run=%t poll_interval=%s webhook_port=%d webhook_debounce=%s log_level=%s exclusion_tag=%s instance_count=%d\n",
		redacted.DryRun,
		redacted.PollInterval,
		redacted.WebhookPort,
		redacted.WebhookDebounce,
		redacted.LogLevel,
		redacted.ExclusionTag,
		len(redacted.Instances),
	)

	for _, inst := range redacted.Instances {
		fmt.Fprintf(w, "configured instance: name=%s type=%s url=%s api_key=%s\n",
			inst.Name, inst.Type, inst.URL, inst.APIKey)
	}
}
