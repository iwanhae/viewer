package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func ownershipBlob(t *testing.T, s *Store) {
	t.Helper()
	if err := s.UpsertBlob(context.Background(), Blob{Hash: "owned", SizeBytes: 1, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
}

func localClaim(t *testing.T, s *Store, until time.Time) LocalEmbeddingClaim {
	t.Helper()
	claims, err := s.ClaimLocalEmbeddings(context.Background(), 1, until)
	if err != nil || len(claims) != 1 || claims[0].Token == "" {
		t.Fatalf("local claim=%v err=%v", claims, err)
	}
	return claims[0]
}

func localReady(claim LocalEmbeddingClaim) LocalEmbeddingResult {
	return LocalEmbeddingResult{Token: claim.Token, EmbeddingResult: EmbeddingResult{
		Hash: claim.Hash, Status: EmbeddingStatusReady, Vector: vec768(1),
	}}
}

func TestEmbeddingOwnershipSeparatePaths(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	ownershipBlob(t, s)
	claim := localClaim(t, s, until)
	if rows, err := s.ClaimExternalEmbeddings(ctx, 1, until); err != nil || len(rows) != 0 {
		t.Fatalf("external stole local: %v %v", rows, err)
	}
	if rows, err := s.RenewEmbeddingLeases(ctx, []string{claim.Hash}, until); err != nil || len(rows) != 0 {
		t.Fatalf("external renewed local: %v %v", rows, err)
	}
	if err := s.ReleaseEmbeddingClaims(ctx, []string{claim.Hash}); err != nil {
		t.Fatal(err)
	}
	// Legacy direct catalog helpers are external-only too.
	if applied, rejected, err := s.ApplyEmbeddingResults(ctx, []EmbeddingResult{localReady(claim).EmbeddingResult}); err != nil || len(applied) != 0 || len(rejected) != 1 {
		t.Fatalf("external applied local: %v %v %v", applied, rejected, err)
	}
	if rows, err := s.ReleaseLocalEmbeddingClaims(ctx, []LocalEmbeddingClaim{claim}); err != nil || len(rows) != 1 {
		t.Fatalf("local release: %v %v", rows, err)
	}
	if rows, err := s.ClaimExternalEmbeddings(ctx, 1, until); err != nil || len(rows) != 1 {
		t.Fatalf("external claim: %v %v", rows, err)
	}
	if rows, err := s.ClaimLocalEmbeddings(ctx, 1, until); err != nil || len(rows) != 0 {
		t.Fatalf("local stole external: %v %v", rows, err)
	}
	if rows, err := s.ReleaseLocalEmbeddingClaims(ctx, []LocalEmbeddingClaim{claim}); err != nil || len(rows) != 0 {
		t.Fatalf("local released external: %v %v", rows, err)
	}
	if rows, err := s.RenewLocalEmbeddingLeases(ctx, []LocalEmbeddingClaim{claim}, until); err != nil || len(rows) != 0 {
		t.Fatalf("local renewed external: %v %v", rows, err)
	}
}

func TestConcurrentLocalAndExternalEmbeddingClaim(t *testing.T) {
	s := openTestStore(t)
	ownershipBlob(t, s)
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	start := make(chan struct{})
	type outcome struct {
		n   int
		err error
	}
	done := make(chan outcome, 2)
	go func() {
		<-start
		rows, err := s.ClaimLocalEmbeddings(ctx, 1, until)
		done <- outcome{len(rows), err}
	}()
	go func() {
		<-start
		rows, err := s.ClaimExternalEmbeddings(ctx, 1, until)
		done <- outcome{len(rows), err}
	}()
	close(start)
	a, b := <-done, <-done
	if a.err != nil || b.err != nil || a.n+b.n != 1 {
		t.Fatalf("atomic ownership: local/external outcomes=%+v %+v", a, b)
	}
}

func TestExternalEmbeddingRetainsHashOnlyAttemptSemantics(t *testing.T) {
	s := openTestStore(t)
	ownershipBlob(t, s)
	ctx := context.Background()
	for _, until := range []time.Time{time.Now().Add(-time.Hour), time.Now().Add(time.Hour)} {
		if rows, err := s.ClaimExternalEmbeddings(ctx, 1, until); err != nil || len(rows) != 1 {
			t.Fatalf("external claim: %v %v", rows, err)
		}
	}
	// There is intentionally no external attempt identity: an old hash-only
	// result still accepts the successive external attempt, as before.
	if rows, rejected, err := s.FinalizeExternalEmbeddingResults(ctx, []EmbeddingResult{{
		Hash: "owned", Status: EmbeddingStatusReady, Vector: vec768(1),
	}}, nil); err != nil || len(rows) != 1 || len(rejected) != 0 {
		t.Fatalf("legacy external apply: %v %v %v", rows, rejected, err)
	}
}

func TestLocalEmbeddingTokenConditionalOperations(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	ownershipBlob(t, s)
	claim := localClaim(t, s, until)
	for _, token := range []string{"", "wrong-token"} {
		wrong := claim
		wrong.Token = token
		if rows, err := s.RenewLocalEmbeddingLeases(ctx, []LocalEmbeddingClaim{wrong}, until); err != nil || len(rows) != 0 {
			t.Fatalf("wrong-token renewal: %v %v", rows, err)
		}
		if rows, err := s.ReleaseLocalEmbeddingClaims(ctx, []LocalEmbeddingClaim{wrong}); err != nil || len(rows) != 0 {
			t.Fatalf("wrong-token release: %v %v", rows, err)
		}
		if rows, rejected, err := s.FinalizeLocalEmbeddingResults(ctx, []LocalEmbeddingResult{localReady(wrong)}, func(context.Context, []EmbeddingResult) error {
			t.Fatal("wrong token reached vector write")
			return nil
		}); err != nil || len(rows) != 0 || len(rejected) != 1 {
			t.Fatalf("wrong-token apply: %v %v %v", rows, rejected, err)
		}
	}
	if rows, err := s.RenewLocalEmbeddingLeases(ctx, []LocalEmbeddingClaim{claim}, until.Add(time.Hour)); err != nil || len(rows) != 1 {
		t.Fatalf("valid renewal: %v %v", rows, err)
	}
	wrote := false
	if rows, rejected, err := s.FinalizeLocalEmbeddingResults(ctx, []LocalEmbeddingResult{localReady(claim)}, func(context.Context, []EmbeddingResult) error {
		wrote = true
		return nil
	}); err != nil || len(rows) != 1 || len(rejected) != 0 || !wrote {
		t.Fatalf("valid apply: %v %v %v wrote=%v", rows, rejected, err, wrote)
	}
}

func TestStaleLocalEmbeddingNeverReachesVectorWrite(t *testing.T) {
	for _, next := range []string{"expired", "cancelled", "local-reclaim", "external-reclaim", "admin-release"} {
		t.Run(next, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			until := time.Now().Add(time.Hour)
			ownershipBlob(t, s)
			claim := localClaim(t, s, until)
			if next == "cancelled" {
				if rows, err := s.ReleaseLocalEmbeddingClaims(ctx, []LocalEmbeddingClaim{claim}); err != nil || len(rows) != 1 {
					t.Fatalf("cancel: %v %v", rows, err)
				}
			} else {
				if _, err := s.db.Exec(`UPDATE blobs SET embedding_lease_until=0 WHERE hash=?`, claim.Hash); err != nil {
					t.Fatal(err)
				}
			}
			switch next {
			case "local-reclaim":
				if newer := localClaim(t, s, until); newer.Token == claim.Token {
					t.Fatal("reclaim reused token")
				}
			case "external-reclaim":
				if rows, err := s.ClaimExternalEmbeddings(ctx, 1, until); err != nil || len(rows) != 1 {
					t.Fatalf("reclaim: %v %v", rows, err)
				}
			case "admin-release":
				if n, err := s.ReleaseStuckEmbeddingClaims(ctx, false, nil); err != nil || n != 1 {
					t.Fatalf("admin release=%d %v", n, err)
				}
			}
			if rows, err := s.RenewLocalEmbeddingLeases(ctx, []LocalEmbeddingClaim{claim}, until); err != nil || len(rows) != 0 {
				t.Fatalf("stale renewal: %v %v", rows, err)
			}
			if next == "local-reclaim" || next == "external-reclaim" {
				if rows, err := s.ReleaseLocalEmbeddingClaims(ctx, []LocalEmbeddingClaim{claim}); err != nil || len(rows) != 0 {
					t.Fatalf("stale release: %v %v", rows, err)
				}
			}
			for _, status := range []EmbeddingStatus{EmbeddingStatusReady, EmbeddingStatusFailed} {
				result := localReady(claim)
				result.Status = status
				if rows, rejected, err := s.FinalizeLocalEmbeddingResults(ctx, []LocalEmbeddingResult{result}, func(context.Context, []EmbeddingResult) error {
					t.Fatal("stale claim reached vector write")
					return nil
				}); err != nil || len(rows) != 0 || len(rejected) != 1 || rejected[0].Reason != EmbeddingRejectNotClaimed {
					t.Fatalf("stale apply: %v %v %v", rows, rejected, err)
				}
			}
		})
	}
}

func TestEmbeddingFinalizationReservationBlocksOwnershipChanges(t *testing.T) {
	for _, owner := range []string{"local", "external"} {
		t.Run(owner, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			until := time.Now().Add(time.Hour)
			ownershipBlob(t, s)
			claim := localClaim(t, s, until)
			if owner == "external" {
				if _, err := s.ReleaseLocalEmbeddingClaims(ctx, []LocalEmbeddingClaim{claim}); err != nil {
					t.Fatal(err)
				}
				if rows, err := s.ClaimExternalEmbeddings(ctx, 1, until); err != nil || len(rows) != 1 {
					t.Fatalf("external claim: %v %v", rows, err)
				}
			}
			write := func(context.Context, []EmbeddingResult) error {
				// Expiry during network work cannot make a reservation reclaimable.
				// Nested DB calls also prove no connection/transaction spans write.
				if _, err := s.db.Exec(`UPDATE blobs SET embedding_lease_until=0 WHERE hash=?`, claim.Hash); err != nil {
					t.Fatal(err)
				}
				if rows, err := s.ReleaseLocalEmbeddingClaims(ctx, []LocalEmbeddingClaim{claim}); err != nil || len(rows) != 0 {
					t.Fatalf("cancel reserved claim: %v %v", rows, err)
				}
				if err := s.ReleaseEmbeddingClaims(ctx, []string{claim.Hash}); err != nil {
					t.Fatal(err)
				}
				for _, expiredOnly := range []bool{true, false} {
					if n, err := s.ReleaseStuckEmbeddingClaims(ctx, expiredOnly, nil); err != nil || n != 0 {
						t.Fatalf("admin released reservation: %d %v", n, err)
					}
				}
				if rows, err := s.ClaimLocalEmbeddings(ctx, 1, until); err != nil || len(rows) != 0 {
					t.Fatalf("local reclaimed reservation: %v %v", rows, err)
				}
				if rows, err := s.ClaimExternalEmbeddings(ctx, 1, until); err != nil || len(rows) != 0 {
					t.Fatalf("external reclaimed reservation: %v %v", rows, err)
				}
				state, err := readRecoveryTargetState(ctx, s.db, claim.Hash, "embedding")
				if err != nil {
					t.Fatal(err)
				}
				if eligible, reason := recoveryEligibility(RecoveryReleaseEmbedding, state, time.Now()); eligible || reason != "finalizing" {
					t.Fatalf("recovery allowed reservation: %v %s", eligible, reason)
				}
				if rows, _, err := s.ApplyEmbeddingResults(ctx, []EmbeddingResult{localReady(claim).EmbeddingResult}); err != nil || len(rows) != 0 {
					t.Fatalf("direct helper bypassed reservation: %v %v", rows, err)
				}
				return nil
			}
			var rows []string
			var rejected []RejectedEmbedding
			var err error
			if owner == "local" {
				rows, rejected, err = s.FinalizeLocalEmbeddingResults(ctx, []LocalEmbeddingResult{localReady(claim)}, write)
			} else {
				rows, rejected, err = s.FinalizeExternalEmbeddingResults(ctx, []EmbeddingResult{localReady(claim).EmbeddingResult}, write)
			}
			if err != nil || len(rows) != 1 || len(rejected) != 0 {
				t.Fatalf("reserved finalization: %v %v %v", rows, rejected, err)
			}
		})
	}
}

func TestEmbeddingFinalizationCleansUpOnFailure(t *testing.T) {
	for _, failure := range []string{"error", "panic", "cancelled-context"} {
		t.Run(failure, func(t *testing.T) {
			s := openTestStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ownershipBlob(t, s)
			claim := localClaim(t, s, time.Now().Add(time.Hour))
			func() {
				defer func() {
					if rec := recover(); rec != nil && failure != "panic" {
						t.Fatalf("unexpected panic %v", rec)
					}
				}()
				_, _, err := s.FinalizeLocalEmbeddingResults(ctx, []LocalEmbeddingResult{localReady(claim)}, func(context.Context, []EmbeddingResult) error {
					switch failure {
					case "panic":
						panic("vector panic")
					case "cancelled-context":
						cancel()
						return nil
					default:
						return errors.New("vector error")
					}
				})
				if err == nil {
					t.Fatal("expected finalization failure")
				}
			}()
			if rows, err := s.ReleaseLocalEmbeddingClaims(context.Background(), []LocalEmbeddingClaim{claim}); err != nil || len(rows) != 1 {
				t.Fatalf("reservation not cleared: %v %v", rows, err)
			}
		})
	}
}

// Fixtures that rewind user_version must rewind this additive schema too.
func dropEmbeddingOwnershipColumns(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`DROP INDEX idx_blobs_embedding_finalizing`); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"embedding_owner", "embedding_token", "embedding_finalizing"} {
		if _, err := s.db.Exec(`ALTER TABLE blobs DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmbeddingOwnershipUpgradeAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ownershipBlob(t, s)
	if _, err := s.ClaimExternalEmbeddings(ctx, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	dropEmbeddingOwnershipColumns(t, s)
	dropEncodingOwnershipColumn(t, s)
	if _, err := s.db.Exec(`PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Upgraded processing rows are external and must survive server restart.
	if counts, err := s.EmbeddingCounts(ctx); err != nil || counts.Processing != 1 {
		t.Fatalf("external lease lost on upgrade: %+v %v", counts, err)
	}
	// Explicit legacy operator recovery, not startup, releases that lease.
	if _, err := s.ReleaseStuckEmbeddingClaims(ctx, false, nil); err != nil {
		t.Fatal(err)
	}
	claim := localClaim(t, s, time.Now().Add(time.Hour))
	// Simulate a crash after reservation and before vector/bookkeeping completion.
	if _, _, err := s.reserveEmbeddingResults(ctx, []EmbeddingResult{localReady(claim).EmbeddingResult}, []string{claim.Token}, "abandoned"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	newer := localClaim(t, s, time.Now().Add(time.Hour))
	if newer.Token == claim.Token {
		t.Fatal("startup reused local token")
	}
	if rows, rejected, err := s.FinalizeLocalEmbeddingResults(ctx, []LocalEmbeddingResult{localReady(claim)}, func(context.Context, []EmbeddingResult) error {
		t.Fatal("pre-restart result reached vector write")
		return nil
	}); err != nil || len(rows) != 0 || len(rejected) != 1 {
		t.Fatalf("pre-restart apply: %v %v %v", rows, rejected, err)
	}
}
