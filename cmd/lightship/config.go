package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/lightshipHQ/lightship/internal/httpapi"
)

type serveConfig struct {
	DatabaseURL       string
	ClickHouseDSN     string
	AdminPasswordHash string
	Address           string
	ModelTTL          time.Duration
	AuditRetention    int
	Limits            httpapi.ResourceLimits
	Demo              *httpapi.DemoConfig
}

func loadServeConfig() (serveConfig, error) {
	return readServeConfig(os.Getenv)
}

func readServeConfig(getenv func(string) string) (serveConfig, error) {
	cfg := serveConfig{
		DatabaseURL:       getenv("DATABASE_URL"),
		ClickHouseDSN:     getenv("CLICKHOUSE_DSN"),
		AdminPasswordHash: getenv("LIGHTSHIP_ADMIN_PASSWORD_HASH"),
		Address:           getenv("LIGHTSHIP_ADDR"),
	}
	if cfg.DatabaseURL == "" {
		return serveConfig{}, errors.New("DATABASE_URL is required")
	}
	if cfg.ClickHouseDSN == "" {
		return serveConfig{}, errors.New("CLICKHOUSE_DSN is required")
	}
	if cfg.Address == "" {
		if port := getenv("PORT"); port != "" {
			cfg.Address = ":" + port
		} else {
			cfg.Address = ":8080"
		}
	}

	var err error
	if cfg.ModelTTL, err = configDuration(getenv, "LIGHTSHIP_MODEL_TTL", time.Minute); err != nil {
		return serveConfig{}, err
	}
	if cfg.AuditRetention, err = configPositiveInt(getenv, "LIGHTSHIP_AUDIT_RETENTION_DAYS", 90); err != nil {
		return serveConfig{}, err
	}
	if cfg.Limits.LoginConcurrency, err = configPositiveInt(getenv, "LIGHTSHIP_LOGIN_CONCURRENCY", 4); err != nil {
		return serveConfig{}, err
	}
	if cfg.Limits.QueryConcurrency, err = configPositiveInt(getenv, "LIGHTSHIP_QUERY_CONCURRENCY", 8); err != nil {
		return serveConfig{}, err
	}
	if cfg.Limits.QueryTimeout, err = configDuration(getenv, "LIGHTSHIP_QUERY_TIMEOUT", 30*time.Second); err != nil {
		return serveConfig{}, err
	}
	if cfg.Limits.MaxQueryWindow, err = configDuration(getenv, "LIGHTSHIP_MAX_QUERY_WINDOW", 31*24*time.Hour); err != nil {
		return serveConfig{}, err
	}
	if cfg.Demo, err = readDemoConfig(getenv); err != nil {
		return serveConfig{}, err
	}
	return cfg, nil
}

func (c serveConfig) serverOptions() []httpapi.Option {
	options := []httpapi.Option{httpapi.WithResourceLimits(c.Limits)}
	if c.Demo != nil {
		options = append(options, httpapi.WithDemo(*c.Demo))
	}
	return options
}

func readDemoConfig(getenv func(string) string) (*httpapi.DemoConfig, error) {
	enabled, err := configBool(getenv, "LIGHTSHIP_DEMO_MODE", false)
	if err != nil {
		return nil, err
	}
	values := map[string]string{
		"LIGHTSHIP_DEMO_USERNAME":     getenv("LIGHTSHIP_DEMO_USERNAME"),
		"LIGHTSHIP_DEMO_PASSWORD":     getenv("LIGHTSHIP_DEMO_PASSWORD"),
		"LIGHTSHIP_DEMO_DATASET_FROM": getenv("LIGHTSHIP_DEMO_DATASET_FROM"),
		"LIGHTSHIP_DEMO_DATASET_TO":   getenv("LIGHTSHIP_DEMO_DATASET_TO"),
	}
	if !enabled {
		for name, value := range values {
			if value != "" {
				return nil, fmt.Errorf("%s requires LIGHTSHIP_DEMO_MODE=true", name)
			}
		}
		return nil, nil
	}
	if values["LIGHTSHIP_DEMO_USERNAME"] == "" || values["LIGHTSHIP_DEMO_PASSWORD"] == "" {
		return nil, errors.New("demo mode requires LIGHTSHIP_DEMO_USERNAME and LIGHTSHIP_DEMO_PASSWORD")
	}
	from, to := values["LIGHTSHIP_DEMO_DATASET_FROM"], values["LIGHTSHIP_DEMO_DATASET_TO"]
	if (from == "") != (to == "") {
		return nil, errors.New("LIGHTSHIP_DEMO_DATASET_FROM and LIGHTSHIP_DEMO_DATASET_TO must be set together")
	}
	if from != "" {
		lower, lowerErr := time.Parse(time.RFC3339Nano, from)
		upper, upperErr := time.Parse(time.RFC3339Nano, to)
		if lowerErr != nil || upperErr != nil || !lower.Before(upper) {
			return nil, errors.New("demo dataset bounds must be valid RFC3339 timestamps with from before to")
		}
	}
	return &httpapi.DemoConfig{
		Username: values["LIGHTSHIP_DEMO_USERNAME"], Password: values["LIGHTSHIP_DEMO_PASSWORD"],
		DatasetFrom: from, DatasetTo: to,
	}, nil
}

func configDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration, got %q", name, value)
	}
	return duration, nil
}

func configPositiveInt(getenv func(string) string, name string, fallback int) (int, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, value)
	}
	return number, nil
}

func configBool(getenv func(string) string, name string, fallback bool) (bool, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", name, value)
	}
	return enabled, nil
}
