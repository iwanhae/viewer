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

	if received, err := store.MarkEncodingReceived(ctx, job.Hash, "wrong-token", time.Minute); err != nil || received {
		t.Fatalf("wrong-token handoff: received=%v err=%v, want false nil", received, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_lease_until=0 WHERE hash=?`, job.Hash); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if received, err := store.MarkEncodingReceived(ctx, job.Hash, job.Token, time.Minute); err != nil || received {
		t.Fatalf("expired handoff: received=%v err=%v, want false nil", received, err)
	}

	// ClaimEncoding may reclaim an expired leased job, but never a received one.
	claimed, err = store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("reclaim expired encoding: jobs=%+v err=%v", claimed, err)
	}
	job = claimed[0]
	if received, err := store.MarkEncodingReceived(ctx, job.Hash, job.Token, 2*time.Minute); err != nil || !received {
		t.Fatalf("mark received: received=%v err=%v, want true nil", received, err)
	}

	received, err := store.ReceivedEncodings(ctx)
	if err != nil || len(received) != 1 {
		t.Fatalf("list received encodings: jobs=%+v err=%v", received, err)
	}
	if received[0].Status != "received" || received[0].Token != job.Token || received[0].StageKey != job.StageKey {
		t.Fatalf("received job=%+v, want received job preserving lease identity and legacy stage key", received[0])
	}
	if !received[0].LeaseUntil.After(time.Now()) {
		t.Fatalf("received lease expired unexpectedly: %v", received[0].LeaseUntil)
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
	if ok, err := store.MarkEncodingReceived(ctx, receivedJob.Hash, receivedJob.Token, time.Minute); err != nil || !ok {
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
	if ok, err := store.MarkEncodingReceived(ctx, job.Hash, job.Token, time.Minute); err != nil || !ok {
		t.Fatalf("mark received: ok=%v err=%v", ok, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_lease_until=0 WHERE hash=?`, job.Hash); err != nil {
		t.Fatalf("expire old worker lease: %v", err)
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
	if ok, err := store.MarkEncodingReceived(ctx, job.Hash, job.Token, time.Minute); err != nil || !ok {
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
