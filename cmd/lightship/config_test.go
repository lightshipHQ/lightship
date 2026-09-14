package main

import (
	"strings"
	"testing"
	"time"
)

func configSource(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func requiredConfig() map[string]string {
	return map[string]string{
		"DATABASE_URL":   "postgres://example",
		"CLICKHOUSE_DSN": "clickhouse://example",
	}
}

func TestServeConfigCollectsRuntimeSettings(t *testing.T) {
	values := requiredConfig()
	values["PORT"] = "9000"
	values["LIGHTSHIP_MODEL_TTL"] = "2m"
	values["LIGHTSHIP_QUERY_CONCURRENCY"] = "12"
	cfg, err := readServeConfig(configSource(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != ":9000" || cfg.ModelTTL != 2*time.Minute ||
		cfg.Limits.QueryConcurrency != 12 || cfg.Demo != nil {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestDemoSettingsRequireExplicitMode(t *testing.T) {
	values := requiredConfig()
	values["LIGHTSHIP_DEMO_USERNAME"] = "admin"
	values["LIGHTSHIP_DEMO_PASSWORD"] = "shown"
	_, err := readServeConfig(configSource(values))
	if err == nil || !strings.Contains(err.Error(), "LIGHTSHIP_DEMO_MODE=true") {
		t.Fatalf("error = %v", err)
	}
}

func TestExplicitDemoModeBuildsIsolatedConfig(t *testing.T) {
	values := requiredConfig()
	values["LIGHTSHIP_DEMO_MODE"] = "true"
	values["LIGHTSHIP_DEMO_USERNAME"] = "admin"
	values["LIGHTSHIP_DEMO_PASSWORD"] = "shown"
	values["LIGHTSHIP_DEMO_DATASET_FROM"] = "2026-09-08T00:00:00Z"
	values["LIGHTSHIP_DEMO_DATASET_TO"] = "2026-09-09T00:00:00Z"
	cfg, err := readServeConfig(configSource(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Demo == nil || cfg.Demo.Username != "admin" || cfg.Demo.DatasetTo == "" {
		t.Fatalf("demo config = %#v", cfg.Demo)
	}
}
