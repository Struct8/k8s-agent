package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// How often the address is re-announced. This is not a keepalive, it is
// reconciliation. The other side stores the address, and this agent is the only
// source of truth about where it is -- re-announcing brings the record back on
// its own after anything that removed it (a restored backup, a recreated
// cluster record, a changed route hostname).
const announceInterval = 5 * time.Minute

type agentEndpointBody struct {
	StatusURL   string `json:"statusUrl"`
	StatusToken string `json:"statusToken"`
}

// announceEndpoint tells Struct8 where this agent can be reached.
//
// Without it there is no address to query for status or metrics, and the
// Deployment comes up perfectly while the screen never gets an answer.
//
// Authenticated with CLUSTER_API_KEY, the same credential the push uses (see
// pushBatch): that is the key proving WHICH cluster is speaking. The status
// token travels in the body because it is what the caller will use later, in
// the opposite direction.
func announceEndpoint(ctx context.Context, httpClient *http.Client, cfg Config) error {
	body, err := json.Marshal(agentEndpointBody{
		StatusURL:   cfg.StatusPublicURL,
		StatusToken: cfg.AuthToken,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WorkerBaseURL+"/v1/k8s-agent-endpoint", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.ClusterAPIKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("responded %d: %s", resp.StatusCode, string(detail))
	}
	return nil
}

// runAnnounceLoop runs only when both a public address and a token exist.
//
// Missing either, there is nothing to announce: with no token the agent is on
// the loopback (loadConfig refuses otherwise), and with no public URL there is
// no path from outside in.
func runAnnounceLoop(ctx context.Context, cfg Config) {
	if cfg.StatusPublicURL == "" || cfg.AuthToken == "" {
		log.Printf("[announce] no public status address configured; this cluster cannot be queried")
		return
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(announceInterval)
	defer ticker.Stop()

	announce := func() {
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := announceEndpoint(callCtx, httpClient, cfg); err != nil {
			log.Printf("[announce] failed to report where this agent is: %v", err)
			return
		}
		log.Printf("[announce] endpoint announced: %s", cfg.StatusPublicURL)
	}

	// Immediately at start, not only on the first tick: after a restart the
	// address may have changed, and waiting five minutes would leave the screen
	// without an answer for that whole time.
	announce()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			announce()
		}
	}
}
