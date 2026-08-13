package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults applied to any config value absent from the YAML file.
const (
	defaultPollInterval    = 24 * time.Hour
	defaultWebhookPort     = 9898
	defaultWebhookDebounce = 45 * time.Second
	defaultLogLevel        = "info"
	defaultExclusionTag    = "cutoffarr-exclude"

	// defaultReverseScanRemonitor is spelled out as a named constant rather
	// than left implicit in Go's bool zero value, because "the reverse scan
	// writes nothing unless asked" is a safety decision, not an accident of
	// the language.
	defaultReverseScanRemonitor = false

	// defaultGUIActions is [v2.2] the same statement for the action system:
	// a human-clicked per-finding button may perform the one operation it
	// names (owner's ruling, plan Phase 11 — "forbidden for the AUTOMATION to
	// do, not for the human"), but ONLY once someone has said so in the
	// config. Named, not left to the zero value, for the same reason as
	// above: this is the switch that decides whether this program is allowed
	// to move a file at all.
	defaultGUIActions = false
)

const redactedPlaceholder = "<redacted>"

// Config is the fully parsed, defaulted, and validated cutoffarr configuration.
type Config struct {
	DryRun          bool
	PollInterval    time.Duration
	WebhookPort     int
	WebhookDebounce time.Duration
	LogLevel        string
	ExclusionTag    string

	// ReverseScanRemonitor is the Phase 10 reverse scan's write switch, and
	// the ONLY thing that lets that scan write anything at all. Default false
	// (report-only): the reverse scan reports items that are unmonitored while
	// still failing the criteria, and composes no write call whatsoever. With
	// it true, re-monitoring obeys dry_run and the exclusion tag exactly like
	// the forward path.
	//
	// It defaults to false rather than to the ordinary "absent means the safe
	// value" reasoning alone: an existing config written before this phase
	// existed must keep behaving exactly as it did, and re-monitoring is a
	// write in the opposite direction from everything this project did before,
	// so it is opt-in in the strongest sense the config has.
	ReverseScanRemonitor bool

	// GUIActions is [v2.2] the master switch for the human-clicked action
	// system (actions.go): per-finding buttons that trash a file, merge a
	// case-twin directory, or re-monitor one item. Default false.
	//
	// It changes nothing about the PERMANENT rule it sits beside: no sweep, no
	// webhook, no reconciliation, no startup path may ever reach an executor,
	// with this flag true or false — the executors are reachable only from the
	// action endpoint handler, which only a human POST can arrive at (see
	// actions.go's own structural pin). What this flag decides is narrower and
	// entirely about the human's own authority: whether cutoffarr will act as
	// their hand at all.
	//
	// With it false the endpoint answers 403 with the reason stated, and the
	// GUI renders every button disabled carrying that same reason — never
	// silently absent, since a button that vanished would leave a human
	// believing the finding is not actionable rather than that they have not
	// yet opted in. dry_run remains independently binding on top of it: the
	// gate itself is switchRefusal (actions.go), and the full matrix of the
	// three switches is the README's "Acting on findings" switches table.
	GUIActions bool

	Instances []Instance
}

// Instance is a single Sonarr or Radarr instance to reconcile against.
type Instance struct {
	Name   string
	Type   string
	URL    string
	APIKey string

	// MediaRootMap is Phase 11's opt-in switch for the duplicate & orphan file
	// report: arr-side root folder path -> the same root as cutoffarr's own
	// filesystem sees it (identity mapping when the container mounts media at
	// the same path as the *arr, per TRaSH convention). Nil (the key entirely
	// absent from this instance's YAML) means the file report is OFF for this
	// instance — binding controller resolution 1. An explicitly empty map is
	// accepted and treated exactly the same as absent (see validateConfig):
	// there is nothing wrong with the value, there is simply nothing to walk.
	MediaRootMap map[string]string
}

// Redacted returns a copy of the config with every instance's APIKey replaced
// by a fixed placeholder, safe to print or log.
func (c Config) Redacted() Config {
	redacted := make([]Instance, len(c.Instances))
	for i, inst := range c.Instances {
		inst.APIKey = redactedPlaceholder
		redacted[i] = inst
	}
	c.Instances = redacted
	return c
}

// rawConfig mirrors the YAML structure. Optional scalar fields are pointers
// so that "absent from the file" can be distinguished from "explicitly set
// to the zero value" (this matters most for dry_run, which must default to
// true rather than Go's bool zero value of false).
//
// poll_interval and webhook_debounce are captured as raw yaml.Node values
// (not pointers to bool/int/string like the other optional fields above) so
// that both "24h"-style strings and a bare "0" can be read back as text and
// handed to time.ParseDuration ourselves. yaml.v3 would decode into a
// *yaml.Node just as well; the value-typed field is chosen here because an
// undecoded Node has a zero Kind, which is how we detect "absent from file"
// without an extra nil check.
type rawConfig struct {
	DryRun               *bool         `yaml:"dry_run"`
	PollInterval         yaml.Node     `yaml:"poll_interval"`
	WebhookPort          *int          `yaml:"webhook_port"`
	WebhookDebounce      yaml.Node     `yaml:"webhook_debounce"`
	LogLevel             *string       `yaml:"log_level"`
	ExclusionTag         *string       `yaml:"exclusion_tag"`
	ReverseScanRemonitor *bool         `yaml:"reverse_scan_remonitor"`
	GUIActions           *bool         `yaml:"gui_actions"`
	Instances            []rawInstance `yaml:"instances"`
}

type rawInstance struct {
	Name         string            `yaml:"name"`
	Type         string            `yaml:"type"`
	URL          string            `yaml:"url"`
	APIKey       string            `yaml:"api_key"`
	MediaRootMap map[string]string `yaml:"media_root_map"`
}

// envVarPattern matches only the ${VAR} form, per the plan's binding
// resolution (bare $VAR is intentionally not supported).
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvVars replaces every ${VAR} reference in raw with the value of the
// corresponding environment variable. A referenced-but-unset variable is a
// fatal error naming the variable; each distinct missing variable is named
// only once even if referenced multiple times in the file.
func expandEnvVars(raw string) (string, error) {
	var missing []string
	seenMissing := make(map[string]bool)
	expanded := envVarPattern.ReplaceAllStringFunc(raw, func(match string) string {
		name := match[2 : len(match)-1] // strip the surrounding "${" and "}"
		val, ok := os.LookupEnv(name)
		if !ok {
			if !seenMissing[name] {
				seenMissing[name] = true
				missing = append(missing, name)
			}
			return match
		}
		return val
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable(s) referenced in config but not set: %s", strings.Join(missing, ", "))
	}
	return expanded, nil
}

// LoadConfig reads, expands, parses, defaults, and validates the config file
// at path. Any violation is returned as a single descriptive error; callers
// should treat all errors from LoadConfig as fatal.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	expanded, err := expandEnvVars(string(data))
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	// KnownFields(true) makes an unrecognized key (e.g. a typo like
	// "dryrun" instead of "dry_run") a fatal parse error instead of being
	// silently ignored. A completely empty document (io.EOF from Decode)
	// is not an error: it means every field is absent, same as
	// yaml.Unmarshal's behavior on empty input, and defaults apply below.
	var raw rawConfig
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: parsing yaml: %w", err)
	}

	cfg, err := raw.toConfig()
	if err != nil {
		return nil, err
	}

	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// toConfig applies defaults for every absent field and parses duration
// fields, producing a Config ready for validation.
func (r rawConfig) toConfig() (*Config, error) {
	cfg := &Config{
		DryRun:               true,
		PollInterval:         defaultPollInterval,
		WebhookPort:          defaultWebhookPort,
		WebhookDebounce:      defaultWebhookDebounce,
		LogLevel:             defaultLogLevel,
		ExclusionTag:         defaultExclusionTag,
		ReverseScanRemonitor: defaultReverseScanRemonitor,
		GUIActions:           defaultGUIActions,
	}

	if r.DryRun != nil {
		cfg.DryRun = *r.DryRun
	}

	if r.ReverseScanRemonitor != nil {
		cfg.ReverseScanRemonitor = *r.ReverseScanRemonitor
	}

	if r.GUIActions != nil {
		cfg.GUIActions = *r.GUIActions
	}

	if r.PollInterval.Kind != 0 {
		d, err := time.ParseDuration(r.PollInterval.Value)
		if err != nil {
			return nil, fmt.Errorf("config: invalid poll_interval %q: %w", r.PollInterval.Value, err)
		}
		cfg.PollInterval = d
	}

	if r.WebhookPort != nil {
		cfg.WebhookPort = *r.WebhookPort
	}

	if r.WebhookDebounce.Kind != 0 {
		d, err := time.ParseDuration(r.WebhookDebounce.Value)
		if err != nil {
			return nil, fmt.Errorf("config: invalid webhook_debounce %q: %w", r.WebhookDebounce.Value, err)
		}
		cfg.WebhookDebounce = d
	}

	if r.LogLevel != nil {
		cfg.LogLevel = *r.LogLevel
	}

	if r.ExclusionTag != nil {
		cfg.ExclusionTag = *r.ExclusionTag
	}

	cfg.Instances = make([]Instance, len(r.Instances))
	for i, ri := range r.Instances {
		cfg.Instances[i] = Instance{
			Name:         ri.Name,
			Type:         ri.Type,
			URL:          ri.URL,
			APIKey:       ri.APIKey,
			MediaRootMap: ri.MediaRootMap,
		}
	}

	return cfg, nil
}

// validateConfig enforces every fatal validation rule from the plan. It
// returns the first violation found. An empty instances list is
// intentionally allowed here (it is not fatal); callers may still choose to
// log a warning about it.
func validateConfig(cfg *Config) error {
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: unknown log_level %q (must be one of debug, info, warn, error)", cfg.LogLevel)
	}

	if cfg.PollInterval != 0 && cfg.PollInterval < time.Hour {
		return fmt.Errorf("config: poll_interval %s is below the minimum of 1h (use 0 to disable the sweep)", cfg.PollInterval)
	}

	if cfg.WebhookPort < 1 || cfg.WebhookPort > 65535 {
		return fmt.Errorf("config: webhook_port %d is out of range (must be between 1 and 65535)", cfg.WebhookPort)
	}

	// Phase 8 made webhook_debounce load-bearing: it is the only thing bounding
	// how many full instance scans a burst of webhooks costs (a webhook cycle
	// is a full-evidence evaluation, by binding ruling). A NEGATIVE value would
	// put every key's deadline in the past on arrival, turning a 24-episode
	// season-pack import into 24 full library evaluations — the precise
	// behavior the debounce exists to prevent, and one that would also read the
	// *arr mid-import, before it has finished its own database writes.
	//
	// Zero is allowed and means "evaluate as soon as the loop sees it", which
	// is a legitimate choice for a small library; unlike poll_interval there is
	// no floor, because the cost of a short debounce is bounded by how often an
	// *arr actually imports something.
	if cfg.WebhookDebounce < 0 {
		return fmt.Errorf("config: webhook_debounce %s must not be negative (use 0 to evaluate without waiting)", cfg.WebhookDebounce)
	}

	// FIX 8 (controller-mandated correction after the initial Phase 3
	// review): exclusion_tag is now load-bearing (the decision engine's
	// rule 4 resolves it to a tag id), and an explicitly empty string
	// would silently disable that rule for every movie in every instance
	// with no indication that happened — an empty label can never match a
	// real Radarr tag, so it's indistinguishable in the resulting logs
	// from a deliberately unset exclusion tag. cfg.ExclusionTag is only
	// ever "" here if the YAML explicitly set exclusion_tag: "" — an
	// absent key defaults to defaultExclusionTag in toConfig before this
	// runs, never to the empty string — so this check unambiguously
	// targets the explicit-empty-string case only.
	//
	// FIX 3 (controller-mandated correction after the whole-branch
	// review): trimmed before the emptiness check (and the trimmed value
	// stored back into cfg.ExclusionTag) so a whitespace-only value like
	// "  " — which is all-whitespace-equivalent to "" for this purpose,
	// since it can no more match a real Radarr tag label than an empty
	// string can — doesn't slip past the check untouched. Storing the
	// trimmed value also protects resolveExclusionTagID's case-insensitive
	// strings.EqualFold match (decision.go), which does not itself strip
	// whitespace: a label with accidental surrounding padding would
	// otherwise silently fail to match the real tag's label, the same
	// class of silent-disable this whole check exists to prevent.
	cfg.ExclusionTag = strings.TrimSpace(cfg.ExclusionTag)
	if cfg.ExclusionTag == "" {
		return fmt.Errorf("config: exclusion_tag must not be empty (omit it entirely to use the default %q)", defaultExclusionTag)
	}

	seenNames := make(map[string]bool, len(cfg.Instances))
	for i, inst := range cfg.Instances {
		if inst.Name == "" {
			return fmt.Errorf("config: instances[%d]: name is required", i)
		}
		if inst.Type == "" {
			return fmt.Errorf("config: instance %q: type is required", inst.Name)
		}
		if inst.URL == "" {
			return fmt.Errorf("config: instance %q: url is required", inst.Name)
		}
		if inst.APIKey == "" {
			return fmt.Errorf("config: instance %q: api_key is required", inst.Name)
		}
		if seenNames[inst.Name] {
			return fmt.Errorf("config: duplicate instance name %q", inst.Name)
		}
		seenNames[inst.Name] = true

		if inst.Type != "radarr" && inst.Type != "sonarr" {
			return fmt.Errorf("config: instance %q: unknown type %q (must be radarr or sonarr)", inst.Name, inst.Type)
		}

		parsed, err := url.Parse(inst.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("config: instance %q: invalid url %q (must be an absolute http or https URL)", inst.Name, inst.URL)
		}

		// media_root_map (Phase 11, binding controller resolution 1): the
		// "non-map" half of present-but-invalid is already fatal for free —
		// rawInstance.MediaRootMap is typed map[string]string, so yaml.v3's
		// KnownFields decode above refuses a scalar value there exactly the
		// way it refuses a non-boolean reverse_scan_remonitor. What is left to
		// check here is the shape of a value that DID decode as a map: every
		// key and value must be non-empty (after trimming whitespace-only
		// values, the same reasoning applied to exclusion_tag above) and
		// absolute (server filesystems are Linux, so "absolute" means
		// starting with "/" — path.IsAbs, not filepath.IsAbs, since this
		// project's *arr paths are never Windows paths regardless of what OS
		// cutoffarr itself is built for). An empty map is valid and left
		// alone: it is treated as identical to the key being entirely absent
		// (filereport.go), not as a malformed value.
		for arrPath, diskPath := range inst.MediaRootMap {
			if strings.TrimSpace(arrPath) == "" {
				return fmt.Errorf("config: instance %q: media_root_map has an empty key (arr-side root path)", inst.Name)
			}
			if strings.TrimSpace(diskPath) == "" {
				return fmt.Errorf("config: instance %q: media_root_map[%q] is empty (cutoffarr-side path)", inst.Name, arrPath)
			}
			if !path.IsAbs(arrPath) {
				return fmt.Errorf("config: instance %q: media_root_map key %q must be an absolute path", inst.Name, arrPath)
			}
			if !path.IsAbs(diskPath) {
				return fmt.Errorf("config: instance %q: media_root_map[%q] value %q must be an absolute path", inst.Name, arrPath, diskPath)
			}
		}
	}

	return nil
}
