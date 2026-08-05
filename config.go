package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Agent configuration -- every value arrives as an environment variable, with
// the credentials delivered through a Kubernetes Secret. Nothing is compiled
// into the binary: the same image serves any cluster, distinguished only by
// the CLUSTER_ID and CLUSTER_API_KEY injected when it is deployed.
type Config struct {
	ClusterID       string
	ClusterAPIKey   string
	WorkerBaseURL   string
	PushInterval    time.Duration
	ListenAddr      string
	StatusAuthToken string
	StatusPublicURL string
	RetentionHours  int
	MaxSeries       int
}

func loadConfig() (Config, error) {
	cfg := Config{
		ClusterID:       os.Getenv("CLUSTER_ID"),
		ClusterAPIKey:   os.Getenv("CLUSTER_API_KEY"),
		WorkerBaseURL:   os.Getenv("WORKER_BASE_URL"),
		ListenAddr:      getEnvDefault("LISTEN_ADDR", "127.0.0.1:8080"),
		StatusAuthToken: os.Getenv("STATUS_AUTH_TOKEN"),
		StatusPublicURL: strings.TrimSpace(os.Getenv("STATUS_PUBLIC_URL")),
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

	// Publishing the port is a CONSEQUENCE of having a token, never the default.
	// With no token the agent stays on the loopback, reachable only by the tunnel
	// sidecar that shares the Pod's network namespace (see README.md).
	//
	// Refusing here -- rather than serving unauthenticated -- is what closes the
	// window: the Pod crash-loops, which is loud, instead of coming up healthy
	// while exposing cluster state to anyone who reaches the route.
	if !isLoopbackAddr(cfg.ListenAddr) && cfg.StatusAuthToken == "" {
		return cfg, fmt.Errorf(
			"refusing to listen on %q without STATUS_AUTH_TOKEN: an address outside the loopback is reachable from outside the Pod",
			cfg.ListenAddr,
		)
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

	// How much history is kept in memory. 48 h in five-minute buckets is 576
	// points per series; what sizes the Pod is that number times MAX_SERIES.
	cfg.RetentionHours = 48
	if raw := os.Getenv("RETENTION_HOURS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return cfg, fmt.Errorf("invalid RETENTION_HOURS (minimum 1): %q", raw)
		}
		cfg.RetentionHours = parsed
	}

	// Series ceiling: a large cluster degrades -- dropping the least recently
	// written series -- instead of dying of OOM and taking status down with it.
	cfg.MaxSeries = 5000
	if raw := os.Getenv("MAX_SERIES"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return cfg, fmt.Errorf("invalid MAX_SERIES (minimum 1): %q", raw)
		}
		cfg.MaxSeries = parsed
	}

	return cfg, nil
}

// isLoopbackAddr decides whether a `host:port` listen address stays confined to
// the Pod.
//
// An EMPTY host (":8080") is not loopback: it means every interface, which is
// exactly the dangerous case written in a way that looks harmless.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// With no recognisable port there is no way to claim this is safe, and
		// the safe answer here is "not loopback".
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
