package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

// Overwritten at build time by the Dockerfile (-ldflags "-X main.agentVersion=...")
// with the same tag the image was published under. It stays "dev" for a local
// `go build`, which is the honest answer there.
//
// This exists because nothing else in the system can answer "which build is
// running". The image reference in the manifest answers which build was
// REQUESTED: a moving tag, a cached layer, or a Pod that was never replaced all
// break that equality, and every one of those failures looks identical from
// outside -- an agent that answers every request, correctly, using old code.
var agentVersion = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// First line of the log, before anything can fail. Reading the Pod's log is
	// then enough to tell an old build from a new one.
	log.Printf("[agent] version %s", agentVersion)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	client, err := buildDynamicClient()
	if err != nil {
		log.Fatalf("could not build the Kubernetes client (running outside a Pod?): %v", err)
	}

	// Separate from the dynamic client because it fails for a different reason
	// and at a different time: the dynamic client only needs the in-cluster
	// config, while this one talks to the discovery API. Failing here is fatal
	// for the same reason -- without it every CRD query answers "unknown_kind",
	// which the caller draws exactly like "not deployed".
	mapper, err := buildRESTMapper()
	if err != nil {
		log.Fatalf("could not build the discovery RESTMapper: %v", err)
	}

	// nil when no address is configured, and that is a running state rather than
	// a failure: the metrics endpoint says what is missing and status is
	// untouched. Metrics are one capability of this agent, not its licence to
	// start.
	prom := newPrometheusClient(cfg.PrometheusURL)
	if prom == nil {
		log.Print("[metrics] no PROMETHEUS_URL: charts will report that this agent has no Prometheus configured")
	} else {
		log.Printf("[metrics] queries will be sent to %s", cfg.PrometheusURL)
	}

	// Authentication exists only when there is a token. Without one, loadConfig
	// has already guaranteed the listener is on the loopback -- reachable only
	// from inside this Pod. Wrapping anyway
	// with an empty token would be worse than not wrapping:
	// `requireBearer("")` accepts a request carrying NO header at all, which
	// looks like protection and is not.
	protected := func(h http.HandlerFunc) http.HandlerFunc {
		if cfg.AuthToken == "" {
			return h
		}
		return requireBearer(cfg.AuthToken, h)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", protected(newStatusHandler(client, mapper)))
	mux.HandleFunc("/metrics-query", protected(newMetricsQueryHandler(prom)))
	// Deliberately outside `protected`: this is the kubelet's probe, and it
	// carries no token. The version it returns is the same string already
	// printed to the log -- public in the image tag either way, and never
	// reachable from outside the Pod unless the operator published the port.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"version": agentVersion})
	})

	// By default this listens on the Pod's loopback only, and nothing outside the
	// Pod can reach it. Serving on a routable address is opt-in and requires
	// AUTH_TOKEN -- loadConfig refuses the combination without it, so there is no
	// state in which this port is published and unauthenticated.
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("[status] listening on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("status HTTP server stopped: %v", err)
		}
	}()

	go runAnnounceLoop(ctx, cfg)

	<-ctx.Done()
	log.Println("shutting down (signal received)...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}
