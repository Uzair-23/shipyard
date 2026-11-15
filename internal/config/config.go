// Package config loads Shipyard's runtime configuration from environment variables.
package config

import "errors"

// Config holds settings shared by the Shipyard roles.
type Config struct {
	DatabaseURL string // required
	HTTPAddr    string
	BaseDomain  string // dashboard/API host, e.g. shipyard.localhost
	AppsDomain  string // apps host suffix, e.g. apps.localhost
}

// Load reads configuration using getenv (usually os.Getenv) and fails fast
// when a required variable is missing.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL: getenv("SHIPYARD_DATABASE_URL"),
		HTTPAddr:    orDefault(getenv("SHIPYARD_HTTP_ADDR"), ":8080"),
		BaseDomain:  orDefault(getenv("SHIPYARD_BASE_DOMAIN"), "shipyard.localhost"),
		AppsDomain:  orDefault(getenv("SHIPYARD_APPS_DOMAIN"), "apps.localhost"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("SHIPYARD_DATABASE_URL is required")
	}
	return cfg, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
