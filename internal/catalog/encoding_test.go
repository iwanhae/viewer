package catalog

import (
	"context"
	"testing"
	"time"
)

func TestMarkEncodingReceivedRequiresActiveLeaseAndCanReset(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: "received-hash", SizeBytes: 123, ContentType: "image/jpeg"}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}

	claimed, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim encoding: jobs=%+v err=%v", claimed, err)
	}
	job := claimed[0]

	if received, err := store.MarkEncodingReceived(ctx, job.Hash, "wrong-token"); err != nil || received {
		t.Fatalf("wrong-token handoff: received=%v err=%v, want false nil", received, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_lease_until=0 WHERE hash=?`, job.Hash); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if received, err := store.MarkEncodingReceived(ctx, job.Hash, job.Token); err != nil || received {
		t.Fatalf("expired handoff: received=%v err=%v, want false nil", received, err)
	}

	// ClaimEncoding may reclaim an expired leased job, but never a received one.
	claimed, err = store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("reclaim expired encoding: jobs=%+v err=%v", claimed, err)
	}
	job = claimed[0]
	if received, err := store.MarkEncodingReceived(ctx, job.Hash, job.Token); err != nil || !received {
		t.Fatalf("mark received: received=%v err=%v, want true nil", received, err)
	}

	received, err := store.ReceivedEncodings(ctx)
	if err != nil || len(received) != 1 {
		t.Fatalf("list received encodings: jobs=%+v err=%v", received, err)
	}
	if received[0].Status != "received" || received[0].Token != job.Token || received[0].StageKey != "" {
		t.Fatalf("received job=%+v, want server-owned result without a legacy stage key", received[0])
	}
	if received[0].LeaseUntil.UnixMilli() != 0 {
		t.Fatalf("server-owned result still has a worker lease: %v", received[0].LeaseUntil)
	}
	claimed, err = store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("received job was reclaimed: jobs=%+v err=%v", claimed, err)
	}

	if err := store.ResetReceivedEncoding(ctx, job.Hash, "wrong-token"); err != nil {
		t.Fatalf("reset with wrong token: %v", err)
	}
	stillReceived, err := store.GetEncodingJob(ctx, job.Hash)
	if err != nil || stillReceived.Status != "received" {
		t.Fatalf("wrong-token reset changed job: job=%+v err=%v", stillReceived, err)
	}
	if err := store.ResetReceivedEncoding(ctx, job.Hash, job.Token); err != nil {
		t.Fatalf("reset received job: %v", err)
	}
	reset, err := store.GetEncodingJob(ctx, job.Hash)
	if err != nil {
		t.Fatalf("get reset job: %v", err)
	}
	if reset.Status != "pending" || reset.Token != "" || reset.StageKey != "" || reset.LeaseUntil.UnixMilli() != 0 {
		t.Fatalf("reset job=%+v, want pending with cleared lease state", reset)
	}
}

func TestBeginEncodingCommitAcceptsReceivedAndLegacyLeases(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, hash := range []string{"received-hash", "legacy-hash"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 123, ContentType: "image/jpeg"}); err != nil {
			t.Fatalf("upsert %s: %v", hash, err)
		}
	}
	claimed, err := store.ClaimEncoding(ctx, 2, time.Minute)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim encodings: jobs=%+v err=%v", claimed, err)
	}
	byHash := make(map[string]EncodingJob, len(claimed))
	for _, job := range claimed {
		byHash[job.Hash] = job
	}
	receivedJob := byHash["received-hash"]
	legacyJob := byHash["legacy-hash"]
	if ok, err := store.MarkEncodingReceived(ctx, receivedJob.Hash, receivedJob.Token); err != nil || !ok {
		t.Fatalf("mark received: ok=%v err=%v", ok, err)
	}

	for _, job := range []EncodingJob{receivedJob, legacyJob} {
		ok, err := store.BeginEncodingCommit(ctx, job.Hash, job.Token, 456, time.Minute)
		if err != nil || !ok {
			t.Errorf("begin commit for %s (%s): ok=%v err=%v, want true nil", job.Hash, job.Status, ok, err)
			continue
		}
		updated, err := store.GetEncodingJob(ctx, job.Hash)
		if err != nil || updated.Status != "committing" {
			t.Errorf("committed job %s=%+v err=%v, want committing", job.Hash, updated, err)
		}
		if ok, err := store.BeginEncodingCommit(ctx, job.Hash, job.Token, 456, time.Minute); err != nil || ok {
			t.Errorf("duplicate begin commit for %s: ok=%v err=%v, want false nil", job.Hash, ok, err)
		}
	}
}

func TestBeginEncodingCommitDoesNotExpireServerOwnedReceivedJob(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: "received-hash", SizeBytes: 123, ContentType: "image/jpeg"}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	claimed, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim encoding: jobs=%+v err=%v", claimed, err)
	}
	job := claimed[0]
	if ok, err := store.MarkEncodingReceived(ctx, job.Hash, job.Token); err != nil || !ok {
		t.Fatalf("mark received: ok=%v err=%v", ok, err)
	}
	if ok, err := store.BeginEncodingCommit(ctx, job.Hash, job.Token, 123, time.Minute); err != nil || !ok {
		t.Fatalf("server-owned received result was rejected after worker lease expiry: ok=%v err=%v", ok, err)
	}
}

func TestSkipEncodingAcceptsReceivedJob(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: "webp-hash", SizeBytes: 321, ContentType: "image/jpeg"}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	claimed, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim encoding: jobs=%+v err=%v", claimed, err)
	}
	job := claimed[0]
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_lease_until=0 WHERE hash=?`, job.Hash); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if ok, err := store.SkipEncoding(ctx, job.Hash, job.Token, "skipped", "image/webp", "", 321); err != nil || ok {
		t.Fatalf("skip expired lease: ok=%v err=%v, want false nil", ok, err)
	}
	claimed, err = store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("reclaim expired encoding: jobs=%+v err=%v", claimed, err)
	}
	job = claimed[0]
	if ok, err := store.MarkEncodingReceived(ctx, job.Hash, job.Token); err != nil || !ok {
		t.Fatalf("mark received: ok=%v err=%v", ok, err)
	}

	if ok, err := store.SkipEncoding(ctx, job.Hash, "stale-token", "skipped", "image/webp", "", 321); err != nil || ok {
		t.Fatalf("skip with stale token: ok=%v err=%v, want false nil", ok, err)
	}
	if current, err := store.GetEncodingJob(ctx, job.Hash); err != nil || current.Status != "received" {
		t.Fatalf("stale token changed received job: job=%+v err=%v", current, err)
	}

	if ok, err := store.SkipEncoding(ctx, job.Hash, job.Token, "skipped", "image/webp", "", 321); err != nil || !ok {
		t.Fatalf("skip received job: ok=%v err=%v, want true nil", ok, err)
	}
	current, err := store.GetEncodingJob(ctx, job.Hash)
	if err != nil {
		t.Fatalf("get skipped job: %v", err)
	}
	blob, err := store.GetBlob(ctx, job.Hash)
	if err != nil {
		t.Fatalf("get skipped blob: %v", err)
	}
	if current.Status != "skipped" || blob.ContentType != "image/webp" || blob.SizeBytes != 321 || current.LeaseUntil.UnixMilli() != 0 {
		t.Fatalf("skipped job=%+v blob=%+v, want terminal WebP state with lease cleared", current, blob)
	}
}

// encodingRawState reads the raw encoding row back, including the source size
// bookkeeping that EncodingJob does not surface but ResetStuckEncodings must
// not destroy on committing rows.
func encodingRawState(t *testing.T, store *Store, hash string) (status, token, stageKey string, leaseMs, sourceSize int64) {
	t.Helper()
	if err := store.db.QueryRow(
		`SELECT encoding_status, encoding_token, encoding_stage_key, encoding_lease_until, encoding_source_size_bytes FROM blobs WHERE hash = ?`,
		hash,
	).Scan(&status, &token, &stageKey, &leaseMs, &sourceSize); err != nil {
		t.Fatalf("read encoding state for %s: %v", hash, err)
	}
	return status, token, stageKey, leaseMs, sourceSize
}

// TestResetStuckEncodings pins the operator recovery switch: expired leased
// rows and received rows go back to pending with their token/stage key/lease
// cleared, while a live lease and a committing row (with its source-size
// bookkeeping that Recover needs) are left strictly alone.
func TestResetStuckEncodings(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for _, hash := range []string{"leased-expired", "leased-live", "received", "committing"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 1, ContentType: "image/jpeg"}); err != nil {
			t.Fatalf("upsert %s: %v", hash, err)
		}
	}
	// Seeded straight in the database: the claim/receive paths are exercised by
	// their own tests, and this one only needs rows in a given state.
	now := time.Now()
	expiredLease := now.Add(-time.Minute).UnixMilli()
	liveLease := now.Add(10 * time.Minute).UnixMilli()
	seed := []struct {
		hash      string
		status    string
		token     string
		stageKey  string
		lease     int64
		sourceSiz int64
	}{
		{hash: "leased-expired", status: "leased", token: "tok-expired", stageKey: "encoding/leased-expired_tok-expired.webp", lease: expiredLease},
		{hash: "leased-live", status: "leased", token: "tok-live", stageKey: "encoding/leased-live_tok-live.webp", lease: liveLease},
		{hash: "received", status: "received", token: "tok-received"},
		// Committing with an already-lapsed grace lease: still Recover's job.
		{hash: "committing", status: "committing", token: "tok-committing", lease: expiredLease, sourceSiz: 456},
	}
	for _, item := range seed {
		if _, err := store.db.ExecContext(ctx, `
			UPDATE blobs SET encoding_status=?, encoding_token=?, encoding_stage_key=?, encoding_lease_until=?, encoding_source_size_bytes=? WHERE hash=?`,
			item.status, item.token, item.stageKey, item.lease, item.sourceSiz, item.hash); err != nil {
			t.Fatalf("seed %s: %v", item.hash, err)
		}
	}

	counts, err := store.ResetStuckEncodings(ctx)
	if err != nil {
		t.Fatalf("reset stuck encodings: %v", err)
	}
	if counts != (EncodingResetCounts{ReleasedExpiredLeased: 1, ResetReceived: 1}) {
		t.Fatalf("counts=%+v want one expired lease released and one received reset", counts)
	}

	for _, hash := range []string{"leased-expired", "received"} {
		status, token, stageKey, lease, sourceSize := encodingRawState(t, store, hash)
		if status != "pending" || token != "" || stageKey != "" || lease != 0 || sourceSize != 0 {
			t.Fatalf("%s=(%s %q %q %d %d) want pending with cleared token/stage/lease", hash, status, token, stageKey, lease, sourceSize)
		}
	}

	// A live lease means an encoder may still be working: untouched.
	status, token, stageKey, lease, sourceSize := encodingRawState(t, store, "leased-live")
	if status != "leased" || token != "tok-live" || stageKey != "encoding/leased-live_tok-live.webp" || lease != liveLease || sourceSize != 0 {
		t.Fatalf("leased-live=(%s %q %q %d %d) want untouched live lease", status, token, stageKey, lease, sourceSize)
	}
	// A committing row may have an in-flight or unknown-outcome S3 replacement:
	// untouched, including the source size Recover reconciles against.
	status, token, stageKey, lease, sourceSize = encodingRawState(t, store, "committing")
	if status != "committing" || token != "tok-committing" || lease != expiredLease || sourceSize != 456 {
		t.Fatalf("committing=(%s %q %q %d %d) want untouched committing row", status, token, stageKey, lease, sourceSize)
	}

	// A second reset has nothing left to move.
	counts, err = store.ResetStuckEncodings(ctx)
	if err != nil || counts != (EncodingResetCounts{}) {
		t.Fatalf("second reset counts=%+v err=%v want zero", counts, err)
	}
}
