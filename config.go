package main

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// Agent configuration -- every value arrives as an environment variable, with
// the credentials delivered through a Kubernetes Secret. Nothing is compiled
// into the binary: the same image serves any cluster, distinguished only by
// the CLUSTER_ID and CLUSTER_API_KEY injected when it is deployed.
type Config struct {
	ClusterID       string
	ClusterAPIKey   string
	WorkerBaseURL   string
	ListenAddr      string
	AuthToken       string
	StatusPublicURL string

	// Where to send the chart's queries. Optional, and its absence is a state
	// the agent runs in rather than an error: status keeps working and the
	// metrics endpoint answers with what is missing. An agent that refused to
	// start over this would take status down with it, over a feature the
	// operator may not want.
	PrometheusURL string
}

func loadConfig() (Config, error) {
	cfg := Config{
		ClusterID:       os.Getenv("CLUSTER_ID"),
		ClusterAPIKey:   os.Getenv("CLUSTER_API_KEY"),
		WorkerBaseURL:   os.Getenv("WORKER_BASE_URL"),
		ListenAddr:      getEnvDefault("LISTEN_ADDR", "127.0.0.1:8080"),
		AuthToken:       os.Getenv("AUTH_TOKEN"),
		StatusPublicURL: strings.TrimSpace(os.Getenv("STATUS_PUBLIC_URL")),
		PrometheusURL:   strings.TrimSpace(os.Getenv("PROMETHEUS_URL")),
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
	// With no token the agent stays on the loopback, reachable only from inside
	// its own Pod (see README.md).
	//
	// The same token guards both endpoints -- /status and /metrics-query -- which
	// is why it is AUTH_TOKEN and not STATUS_AUTH_TOKEN: the name was written
	// when status was the only thing served from this port.
	//
	// Refusing here -- rather than serving unauthenticated -- is what closes the
	// window: the Pod crash-loops, which is loud, instead of coming up healthy
	// while exposing cluster state to anyone who reaches the route.
	if !isLoopbackAddr(cfg.ListenAddr) && cfg.AuthToken == "" {
		return cfg, fmt.Errorf(
			"refusing to listen on %q without AUTH_TOKEN: an address outside the loopback is reachable from outside the Pod",
			cfg.ListenAddr,
		)
	}

	// PUSH_INTERVAL_SECONDS, RETENTION_HOURS and MAX_SERIES were removed when
	// the agent stopped collecting and storing series (2026-08-06). They sized
	// an in-memory store that no longer exists; Prometheus owns retention now.
	// They are deliberately not rejected if still present in an old manifest:
	// crash-looping over a leftover environment variable would take status down
	// during the very rollout that removes it.

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
