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
	onlyID := fs.Int("only-id", 0, "process only the radarr movie with this id: evaluate, report, and (outside dry-run) write just that one movie (--once only)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// A movie id is always a positive integer, so 0 and negatives are user
	// error rather than something to interpret. Zero matters most: it is
	// also this flag's absent-value sentinel, so accepting "--only-id 0"
	// would silently widen a run the human meant to scope to one movie into
	// a full-library run — in a phase whose entire point is that writes stay
	// narrow. fs.Visit distinguishes "explicitly passed 0" from "not passed
	// at all"; a non-integer value never reaches here, since fs.Parse
	// rejects it above.
	onlyIDSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "only-id" {
			onlyIDSet = true
		}
	})
	if onlyIDSet && *onlyID <= 0 {
		fmt.Fprintf(stderr, "cutoffarr: fatal: --only-id must be a positive radarr movie id, got %d\n", *onlyID)
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
		// library inspection (GET /movie, paged GET /wanted/cutoff) — but
		// only if connectivity actually succeeded; an instance already
		// declared skipped for the cycle must not go on to further calls.
		// Sonarr instances are connectivity-only until Phase 6.
		//
		// Phase 3: feed inspectRadarrLibrary's returned movies/wantedIDs
		// into the decision engine — but only if that fetch itself
		// succeeded and produced a complete (non-partial) result; a
		// partial wanted/cutoff id set (refactor a) must never reach the
		// decision engine, since absence-from-set means "would-unmonitor"
		// and a partial set would manufacture false positives in that
		// dangerous direction.
		//
		// Phase 4: the decision engine also runs the write pass, gated on
		// cfg.DryRun (which --dry-run can force on but never off) and
		// scoped by --only-id. Both are passed down rather than consulted
		// at this level: §2.1 requires the dry-run flag to be checked
		// immediately before each HTTP write call, not just at startup, so
		// the value has to travel all the way to the write site.
		samples := parseSamples(*samplesFlag)
		for _, inst := range cfg.Instances {
			ok := checkInstanceConnectivity(context.Background(), logger, inst)
			if ok && inst.Type == "radarr" {
				movies, wantedIDs, dataOK := inspectRadarrLibrary(context.Background(), logger, inst, samples)
				if dataOK {
					runRadarrDecisionEngine(context.Background(), logger, inst, movies, wantedIDs, cfg.ExclusionTag, *onlyID, cfg.DryRun)
				}
			}
		}
	} else {
		if onlyIDSet {
			logger.Warn("--only-id has no effect without --once: it scopes a single pass, and daemon mode does not run one yet",
				"onlyId", *onlyID)
		}
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
