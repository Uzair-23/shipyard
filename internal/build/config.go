package build

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// ConfigFile is the optional per-app config file at the top of the source.
const ConfigFile = "shipyard.toml"

// AppPort is the port every app listens on inside its container ($PORT).
const AppPort = 8080

const (
	maxConfigBytes  = 64 << 10
	maxStartLen     = 1000
	DefaultHealth   = "/"
	DefaultIdle     = 15 * time.Minute
	minIdleTimeout  = 5 * time.Minute
	maxIdleTimeout  = 60 * time.Minute
	maxHealthPath   = 200
	healthPathChars = `A-Za-z0-9._~!$&'()*+,;=:@%/?-`
)

var healthPathRE = regexp.MustCompile(`^/[` + healthPathChars + `]*$`)

// Config is a validated shipyard.toml (APP-6). Zero Runtime means "detect";
// empty Start means "use the template's default".
type Config struct {
	Runtime     Runtime
	Start       string
	Health      string
	IdleTimeout time.Duration
}

// configFile mirrors the TOML keys users may set.
type configFile struct {
	Runtime     string `toml:"runtime"`
	Start       string `toml:"start"`
	Health      string `toml:"health"`
	IdleTimeout string `toml:"idle_timeout"`
	Port        *int64 `toml:"port"`
}

// ConfigError explains what is wrong with shipyard.toml. Its message is
// safe to show to the user.
type ConfigError struct{ Reason string }

func (e *ConfigError) Error() string { return ConfigFile + ": " + e.Reason }

func configErr(format string, args ...any) error {
	return &ConfigError{Reason: fmt.Sprintf(format, args...)}
}

// ParseConfig parses and validates shipyard.toml, applying defaults for
// health and idle_timeout. A nil or empty input yields the defaults.
func ParseConfig(data []byte) (Config, error) {
	if len(data) > maxConfigBytes {
		return Config{}, configErr("file is larger than %d KB", maxConfigBytes>>10)
	}
	var f configFile
	meta, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&f)
	if err != nil {
		return Config{}, configErr("%v", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		sort.Strings(keys)
		return Config{}, configErr("unknown setting(s) %s (allowed: runtime, start, health, idle_timeout, port)",
			strings.Join(keys, ", "))
	}

	cfg := Config{Runtime: Runtime(f.Runtime), Start: f.Start, Health: DefaultHealth, IdleTimeout: DefaultIdle}

	switch cfg.Runtime {
	case "", Python, Node, Static:
	default:
		return Config{}, configErr("runtime %q is not supported (use python, node or static)", f.Runtime)
	}

	if cfg.Start != "" {
		if strings.TrimSpace(cfg.Start) == "" {
			return Config{}, configErr("start is blank")
		}
		if len(cfg.Start) > maxStartLen {
			return Config{}, configErr("start is longer than %d characters", maxStartLen)
		}
		for _, r := range cfg.Start {
			if r < 0x20 || r == 0x7f {
				return Config{}, configErr("start must be a single line without control characters")
			}
		}
		if cfg.Runtime == Static {
			return Config{}, configErr("start cannot be set for static sites; they are served by Shipyard's file server")
		}
	}

	if f.Health != "" {
		if len(f.Health) > maxHealthPath || !healthPathRE.MatchString(f.Health) || strings.HasPrefix(f.Health, "//") {
			return Config{}, configErr("health must be a URL path such as \"/healthz\" (starting with a single /, at most %d characters, no spaces)", maxHealthPath)
		}
		cfg.Health = f.Health
	}

	if f.IdleTimeout != "" {
		d, err := time.ParseDuration(f.IdleTimeout)
		if err != nil {
			return Config{}, configErr("idle_timeout %q is not a duration (use e.g. \"15m\")", f.IdleTimeout)
		}
		if d < minIdleTimeout || d > maxIdleTimeout {
			return Config{}, configErr("idle_timeout must be between %s and %s", fmtMinutes(minIdleTimeout), fmtMinutes(maxIdleTimeout))
		}
		cfg.IdleTimeout = d
	}

	if f.Port != nil && *f.Port != AppPort {
		return Config{}, configErr("port must be %d: Shipyard routes traffic to port %d, available to your app as $PORT", AppPort, AppPort)
	}
	return cfg, nil
}

func fmtMinutes(d time.Duration) string { return fmt.Sprintf("%dm", int(d.Minutes())) }
