package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Agent configuration -- every value arrives as an environment variable, with
// the credentials delivered through a Kubernetes Secret. Nothing is compiled
// into the binary: the same image serves any cluster, distinguished only by
// the CLUSTER_ID and CLUSTER_API_KEY injected when it is deployed.
type Config struct {
	ClusterID     string
	ClusterAPIKey string
	WorkerBaseURL string
	PushInterval  time.Duration
	ListenAddr    string
}

func loadConfig() (Config, error) {
	cfg := Config{
		ClusterID:     os.Getenv("CLUSTER_ID"),
		ClusterAPIKey: os.Getenv("CLUSTER_API_KEY"),
		WorkerBaseURL: os.Getenv("WORKER_BASE_URL"),
		ListenAddr:    getEnvDefault("LISTEN_ADDR", "127.0.0.1:8080"),
	}

	if cfg.ClusterID == "" {
		return cfg, fmt.Errorf("CLUSTER_ID is required")
	}
	if cfg.ClusterAPIKey == "" {
		return cfg, fmt.Errorf("CLUSTER_API_KEY is required")
	}
	if cfg.WorkerBaseURL == "" {
		return cfg, fmt.Errorf("WORKER_BASE_URL is required (e.g. https://<your-worker>.<account>.workers.dev)")
	}

	intervalSeconds := 20
	if raw := os.Getenv("PUSH_INTERVAL_SECONDS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 5 {
			return cfg, fmt.Errorf("invalid PUSH_INTERVAL_SECONDS (minimum 5): %q", raw)
		}
		intervalSeconds = parsed
	}
	cfg.PushInterval = time.Duration(intervalSeconds) * time.Second

	return cfg, nil
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
