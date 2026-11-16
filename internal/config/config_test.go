package config

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	if _, err := Load(env(nil)); err == nil {
		t.Fatal("expected error when SHIPYARD_DATABASE_URL is missing")
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"SHIPYARD_DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.BaseDomain != "shipyard.localhost" || cfg.AppsDomain != "apps.localhost" ||
		cfg.DataDir != "data" || cfg.Registry != "localhost:5000" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"SHIPYARD_DATABASE_URL": "postgres://x",
		"SHIPYARD_HTTP_ADDR":    ":9000",
		"SHIPYARD_BASE_DOMAIN":  "shipyard.example.dev",
		"SHIPYARD_APPS_DOMAIN":  "apps.example.dev",
		"SHIPYARD_DATA_DIR":     "/var/lib/shipyard",
		"SHIPYARD_REGISTRY":     "registry.internal:5000",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":9000" || cfg.BaseDomain != "shipyard.example.dev" || cfg.AppsDomain != "apps.example.dev" ||
		cfg.DataDir != "/var/lib/shipyard" || cfg.Registry != "registry.internal:5000" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}
