package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/db"
)

// GET /health: the API's readiness (slice 4 decision 72). 200 {"status":"ok"} if the database
// answers SELECT 1 within healthTimeout, else a 503 problem (notReady). Outside the account scope:
// probes call it (the scripts, a load balancer, an orchestrator), not clients acting on an account.
// Logged only when it fails: its route is wrapped in quietOnSuccess. Node: node/src/api/health.ts.

// healthTimeout bounds the check, connecting included: long enough for a healthy database, short
// enough that a probe hears "not ready" before its own timeout gives up on us. Node's
// HEALTH_TIMEOUT_MS is the same.
const healthTimeout = time.Second

type healthRoutes struct {
	db db.Querier
}

type healthOkResponse struct {
	Status string `json:"status"`
}

func (h healthRoutes) health(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := db.Ping(ctx, h.db); err != nil {
		if errors.Is(err, context.Canceled) {
			// The client left: nobody to answer, and not the database's fault, so nothing to log.
			return nil
		}
		loggerFrom(r.Context()).Error("health check failed", "err", err)
		writeProblem(w, notReady())
		return nil
	}
	writeJSON(w, http.StatusOK, "application/json; charset=utf-8", healthOkResponse{Status: "ok"})
	return nil
}
