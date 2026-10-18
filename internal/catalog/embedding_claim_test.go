package catalog

import (
	"context"
	"testing"
	"time"
)

func TestClaimLocalEmbeddingExactTarget(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, hash := range []string{"earlier", "target", "later"} {
		if err := s.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 42, ContentType: "image/jpeg"}); err != nil {
			t.Fatal(err)
		}
	}
	until := time.Now().Add(time.Hour)
	claim, status, err := s.ClaimLocalEmbedding(ctx, "target", "run-item-token", until)
	if err != nil || status != LocalEmbeddingClaimed || claim.Hash != "target" || claim.Token != "run-item-token" || claim.SizeBytes != 42 || claim.EmbeddingStatus != EmbeddingStatusProcessing {
		t.Fatalf("claim=%+v status=%s err=%v", claim, status, err)
	}
	var owner, token string
	var lease int64
	if err := s.db.QueryRow(`SELECT embedding_owner,embedding_token,embedding_lease_until FROM blobs WHERE hash='target'`).Scan(&owner, &token, &lease); err != nil {
		t.Fatal(err)
	}
	if owner != "local" || token != claim.Token || lease != until.UnixMilli() {
		t.Fatalf("ownership=%s token=%s lease=%d", owner, token, lease)
	}
	for _, hash := range []string{"earlier", "later"} {
		blob, err := s.GetBlob(ctx, hash)
		if err != nil || blob.EmbeddingStatus != EmbeddingStatusPending {
			t.Fatalf("non-target %s changed: %+v %v", hash, blob, err)
		}
	}
	if rows, err := s.ReleaseLocalEmbeddingClaims(ctx, []LocalEmbeddingClaim{{Blob: claim.Blob, Token: "wrong"}}); err != nil || len(rows) != 0 {
		t.Fatalf("wrong-token release=%v %v", rows, err)
	}
	if rows, err := s.ReleaseLocalEmbeddingClaims(ctx, []LocalEmbeddingClaim{claim}); err != nil || len(rows) != 1 {
		t.Fatalf("run-token release=%v %v", rows, err)
	}
}

func TestClaimLocalEmbeddingBusyAndMissingDoNotMutate(t *testing.T) {
	for _, state := range []struct {
		name, status, owner, token, finalizing string
		lease                                  int64
	}{
		{"external", "processing", "external", "", "", time.Now().Add(time.Hour).UnixMilli()},
		{"expired-external", "processing", "external", "", "", 0},
		{"local", "processing", "local", "other-token", "", time.Now().Add(time.Hour).UnixMilli()},
		{"expired-local", "processing", "local", "other-token", "", 0},
		{"ready", "ready", "external", "", "", 0},
		{"failed", "failed", "external", "", "", 0},
		{"finalizing", "processing", "local", "other-token", "reservation", 0},
		{"pending-finalizing", "pending", "local", "other-token", "reservation", 0},
		{"missing", "", "", "", "", 0},
	} {
		t.Run(state.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			ownershipBlob(t, s)
			if state.name != "missing" {
				if err := s.UpsertBlob(ctx, Blob{Hash: "target", SizeBytes: 1, ContentType: "image/jpeg"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec(`UPDATE blobs SET embedding_status=?,embedding_owner=?,embedding_token=?,embedding_finalizing=?,embedding_lease_until=? WHERE hash='target'`, state.status, state.owner, state.token, state.finalizing, state.lease); err != nil {
					t.Fatal(err)
				}
			}
			claim, status, err := s.ClaimLocalEmbedding(ctx, "target", "run-token", time.Now().Add(2*time.Hour))
			want := LocalEmbeddingBusy
			if state.name == "missing" {
				want = LocalEmbeddingMissing
			}
			if err != nil || status != want || claim.Hash != "" || claim.Token != "" {
				t.Fatalf("claim=%+v status=%s err=%v", claim, status, err)
			}
			if state.name != "missing" {
				var status, owner, token, finalizing string
				var lease int64
				if err := s.db.QueryRow(`SELECT embedding_status,embedding_owner,embedding_token,embedding_finalizing,embedding_lease_until FROM blobs WHERE hash='target'`).Scan(&status, &owner, &token, &finalizing, &lease); err != nil {
					t.Fatal(err)
				}
				if status != state.status || owner != state.owner || token != state.token || finalizing != state.finalizing || lease != state.lease {
					t.Fatalf("busy target changed: %s %s %s %s %d", status, owner, token, finalizing, lease)
				}
			}
			if blob, err := s.GetBlob(ctx, "owned"); err != nil || blob.EmbeddingStatus != EmbeddingStatusPending {
				t.Fatalf("claimed substitute: %+v %v", blob, err)
			}
		})
	}
}

func TestClaimLocalEmbeddingRequiresTokenAndHonorsCancellation(t *testing.T) {
	s := openTestStore(t)
	ownershipBlob(t, s)
	if _, _, err := s.ClaimLocalEmbedding(context.Background(), "owned", "", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("accepted empty ownership token")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.ClaimLocalEmbedding(ctx, "owned", "token", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("claimed with cancelled context")
	}
	if blob, err := s.GetBlob(context.Background(), "owned"); err != nil || blob.EmbeddingStatus != EmbeddingStatusPending {
		t.Fatalf("invalid claim changed target: %+v %v", blob, err)
	}
}

func TestClaimLocalEmbeddingRacesExternalClaim(t *testing.T) {
	s := openTestStore(t)
	ownershipBlob(t, s)
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	start := make(chan struct{})
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 2)
	go func() {
		<-start
		_, status, err := s.ClaimLocalEmbedding(ctx, "owned", "run-token", until)
		n := 0
		if status == LocalEmbeddingClaimed {
			n = 1
		}
		done <- result{n, err}
	}()
	go func() {
		<-start
		rows, err := s.ClaimExternalEmbeddings(ctx, 1, until)
		done <- result{len(rows), err}
	}()
	close(start)
	a, b := <-done, <-done
	if a.err != nil || b.err != nil || a.n+b.n != 1 {
		t.Fatalf("claim race=%+v %+v", a, b)
	}
}
