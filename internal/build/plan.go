package build

import (
	"bytes"
	"io"
	"time"
)

// Plan is everything decided about a deploy before the image is built. It is
// stored as the deploy's config snapshot.
type Plan struct {
	Runtime     Runtime       `json:"runtime"`
	Start       string        `json:"start,omitempty"`
	Health      string        `json:"health"`
	IdleTimeout time.Duration `json:"idle_timeout_ns"`
	Dockerfile  []byte        `json:"-"`
}

// Prepare turns a source and its shipyard.toml (nil if absent) into a Plan.
// Errors are user-facing: *ConfigError, ErrNoRuntime, ErrAmbiguousRuntime,
// ErrNoStart, or a template error.
func Prepare(src Source, config []byte, appID string) (Plan, error) {
	cfg, err := ParseConfig(config)
	if err != nil {
		return Plan{}, err
	}
	rt := cfg.Runtime
	if rt == "" {
		if rt, err = Detect(src.Paths); err != nil {
			return Plan{}, err
		}
	}
	start := cfg.Start
	if start == "" {
		if start, err = DefaultStart(rt, src); err != nil {
			return Plan{}, err
		}
	}
	df, err := Dockerfile(rt, start, src, appID)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Runtime: rt, Start: start, Health: cfg.Health, IdleTimeout: cfg.IdleTimeout, Dockerfile: df}, nil
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
