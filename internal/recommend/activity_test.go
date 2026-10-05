package recommend

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestWorkerActivityStartsAtZero(t *testing.T) {
	svc := &Service{}
	got := svc.WorkerActivity()
	if got.LastClaimUnixMs != 0 || got.LastRenewUnixMs != 0 || got.LastResultsUnixMs != 0 {
		t.Fatalf("expected zero activity for a fresh service: %+v", got)
	}
}

func TestWorkerActivityNilServiceIsSafe(t *testing.T) {
	var nilService *Service
	got := nilService.WorkerActivity()
	if got.LastClaimUnixMs != 0 || got.LastRenewUnixMs != 0 || got.LastResultsUnixMs != 0 {
		t.Fatalf("unexpected activity for nil service: %+v", got)
	}

	// The entry points must reject a nil service without panicking — the
	// nil check runs before the liveness stamp is written.
	if _, _, err := nilService.ClaimEmbeddings(context.Background(), 1); err == nil {
		t.Fatal("expected ClaimEmbeddings to fail for a nil service")
	}
	if _, _, err := nilService.RenewLeases(context.Background(), []string{"hash"}); err == nil {
		t.Fatal("expected RenewLeases to fail for a nil service")
	}
	if _, _, err := nilService.ApplyEmbeddingResults(context.Background(), nil); err == nil {
		t.Fatal("expected ApplyEmbeddingResults to fail for a nil service")
	}
}

func TestWorkerActivityRecordsAttemptsEvenWhenTheyFail(t *testing.T) {
	// A service without a catalog fails every pipeline call, which is
	// exactly the stalled state the dashboard needs to see as "alive and
	// trying": the stamps must move anyway.
	svc := newTestService(t, nil, nil, nil)
	before := time.Now().UnixMilli()

	if _, _, err := svc.ClaimEmbeddings(context.Background(), 1); err == nil {
		t.Fatal("expected ClaimEmbeddings to fail without a catalog")
	}
	if _, _, err := svc.RenewLeases(context.Background(), []string{"hash"}); err == nil {
		t.Fatal("expected RenewLeases to fail without a catalog")
	}
	if _, _, err := svc.ApplyEmbeddingResults(context.Background(), nil); err == nil {
		t.Fatal("expected ApplyEmbeddingResults to fail without a catalog")
	}

	got := svc.WorkerActivity()
	if got.LastClaimUnixMs < before || got.LastRenewUnixMs < before || got.LastResultsUnixMs < before {
		t.Fatalf("expected all three entry points stamped after failed attempts: %+v", got)
	}
}

func TestWorkerActivityTracksSuccessfulClaim(t *testing.T) {
	cat := newTestCatalog(t)
	svc := newTestService(t, cat, newFakeVectorStore(), nil)

	if _, _, err := svc.ClaimEmbeddings(context.Background(), 1); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got := svc.WorkerActivity().LastClaimUnixMs; got == 0 {
		t.Fatalf("expected LastClaimUnixMs stamped after a successful claim: %+v", svc.WorkerActivity())
	}
}

func TestWorkerActivityJSONShapeIsPinned(t *testing.T) {
	svc := &Service{}
	svc.lastClaimMs.Store(1)
	svc.lastRenewMs.Store(2)
	svc.lastResultsMs.Store(3)

	data, err := json.Marshal(svc.WorkerActivity())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"lastClaimUnixMs":1,"lastRenewUnixMs":2,"lastResultsUnixMs":3}`
	if string(data) != want {
		t.Fatalf("JSON shape drifted:\n got: %s\nwant: %s", data, want)
	}
}
