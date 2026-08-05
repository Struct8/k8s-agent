package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	client, err := buildDynamicClient()
	if err != nil {
		log.Fatalf("could not build the Kubernetes client (running outside a Pod?): %v", err)
	}

	store := newMetricStore(cfg.RetentionHours, cfg.MaxSeries)

	// Authentication exists only when there is a token. Without one, loadConfig
	// has already guaranteed the listener is on the loopback -- reachable solely
	// by the tunnel sidecar sharing the Pod's network namespace. Wrapping anyway
	// with an empty token would be worse than not wrapping:
	// `requireBearer("")` accepts a request carrying NO header at all, which
	// looks like protection and is not.
	protected := func(h http.HandlerFunc) http.HandlerFunc {
		if cfg.StatusAuthToken == "" {
			return h
		}
		return requireBearer(cfg.StatusAuthToken, h)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", protected(newStatusHandler(client)))
	mux.HandleFunc("/metrics-query", protected(newMetricsQueryHandler(store)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Escuta só na interface local do pod -- nunca exposto fora do cluster.
	// O cloudflared roda como um segundo container no MESMO Pod (mesmo
	// network namespace) e alcança isto via 127.0.0.1, e é ELE quem
	// estabelece a conexão de saída pro túnel; este processo nunca abre nada
	// pra fora.
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

	go runMetricsLoop(ctx, client, cfg, store)
	go runAnnounceLoop(ctx, cfg)

	<-ctx.Done()
	log.Println("shutting down (signal received)...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

// Teto da coleta. Um ciclo frio precisa listar Pods e ReplicaSets além das
// métricas, e num cluster grande essas listas são de megabytes -- com os 8s
// fixos de antes o prazo estourava, a coleta virava erro e o ciclo INTEIRO era
// descartado, inclusive as métricas que já tinham sido lidas. Derivado do
// intervalo para nunca invadir o tick seguinte.
func collectBudget(interval time.Duration) time.Duration {
	budget := interval - 2*time.Second
	if budget > 30*time.Second {
		budget = 30 * time.Second
	}
	if budget < 5*time.Second {
		budget = 5 * time.Second
	}
	return budget
}

func runMetricsLoop(ctx context.Context, client dynamic.Interface, cfg Config, store *metricStore) {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(cfg.PushInterval)
	defer ticker.Stop()

	// Construído UMA vez, fora do laço: o cache de donos só serve para alguma
	// coisa se sobreviver entre ciclos (ver owners.go). Um resolver por ciclo
	// releria Pods e ReplicaSets toda vez, que é exatamente o custo que o cache
	// existe para evitar.
	resolver := newOwnerResolver(client)
	budget := collectBudget(cfg.PushInterval)

	collectAndPush := func() {
		collectCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()

		points, unresolved, err := collectWorkloadMetrics(collectCtx, client, resolver)
		if err != nil {
			log.Printf("[metrics] failed to collect from metrics.k8s.io: %v", err)
			return
		}

		points, dropped := sanitizePoints(points)
		if len(dropped) > 0 {
			sample := dropped
			if len(sample) > 3 {
				sample = sample[:3]
			}
			log.Printf("[metrics] dropped %d point(s) whose names the Worker does not accept; examples: %v", len(dropped), sample)
		}
		if unresolved > 0 {
			log.Printf("[metrics] %d Pod(s) had no owning workload resolved in this cycle (likely terminated mid-collection)", unresolved)
		}
		if len(points) == 0 {
			return
		}

		// Stored BEFORE pushing, deliberately: the local store is what the chart
		// reads, and it must not depend on the network to the Worker being up. A
		// failed push hits the `return` below -- if the write came after it, a
		// cluster with no outbound path would have no chart at all, which is the
		// very case keeping the data local is meant to cover.
		store.Append(points, time.Now())

		pushCtx, cancelPush := context.WithTimeout(ctx, 15*time.Second)
		defer cancelPush()
		if err := pushMetrics(pushCtx, httpClient, cfg, points); err != nil {
			log.Printf("[metrics] failed to push to the Worker: %v", err)
			return
		}
		log.Printf("[metrics] pushed %d workloads, %d points", len(points)/2, len(points))
	}

	// Collect once immediately rather than waiting out the first tick, so a
	// restarted agent has data in flight within seconds instead of after a
	// full interval.
	collectAndPush()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collectAndPush()
		}
	}
}
