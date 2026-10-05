package recommend

import (
	"sync/atomic"
	"time"
)

// WorkerActivity is the admin dashboard's liveness view of the embedding
// pipeline: when a worker last attempted each pipeline entry point. The JSON
// shape is a contract with the dashboard — field names are pinned.
type WorkerActivity struct {
	LastClaimUnixMs   int64 `json:"lastClaimUnixMs"`
	LastRenewUnixMs   int64 `json:"lastRenewUnixMs"`
	LastResultsUnixMs int64 `json:"lastResultsUnixMs"`
}

// WorkerActivity reports the last-attempt timestamps recorded at the pipeline
// entry points. A zero value means no attempt since process start, which the
// dashboard renders as "never"; a nil Service reports all zeros, in line with
// the nil guards every other method carries.
func (s *Service) WorkerActivity() WorkerActivity {
	if s == nil {
		return WorkerActivity{}
	}
	return WorkerActivity{
		LastClaimUnixMs:   s.lastClaimMs.Load(),
		LastRenewUnixMs:   s.lastRenewMs.Load(),
		LastResultsUnixMs: s.lastResultsMs.Load(),
	}
}

// noteActivity stamps one pipeline entry-point attempt. It runs before any
// dependency check and records the attempt, not the outcome: the dashboard
// wants "is anyone alive and trying", which a worker whose every call fails
// still answers yes to. Whether the pipeline is actually making progress is
// diagnosed from the catalog's embedding counts, not from these stamps.
func noteActivity(target *atomic.Int64) {
	target.Store(time.Now().UnixMilli())
}
