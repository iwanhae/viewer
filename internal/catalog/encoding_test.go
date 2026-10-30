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

// encodingErrorText reads the raw failure message, the one encoding column
// encodingRawState leaves out; the retry path must clear it and the guards
// must leave it be.
func encodingErrorText(t *testing.T, store *Store, hash string) string {
	t.Helper()
	var errText string
	if err := store.db.QueryRow(`SELECT encoding_error FROM blobs WHERE hash = ?`, hash).Scan(&errText); err != nil {
		t.Fatalf("read encoding error for %s: %v", hash, err)
	}
	return errText
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

	counts, err := store.ResetStuckEncodings(ctx, nil)
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
	counts, err = store.ResetStuckEncodings(ctx, nil)
	if err != nil || counts != (EncodingResetCounts{}) {
		t.Fatalf("second reset counts=%+v err=%v want zero", counts, err)
	}
}

// TestResetFailedEncodings pins the encoding retry path: failed non-WebP rows
// go back to pending with their token, stage key, lease, source size and error
// cleared, while a failed-looking WebP row (a finished conversion wearing a
// legacy status) and rows that are not failed stay untouched. A hash scope
// selects only the listed blobs; an empty one retries every failed row.
func TestResetFailedEncodings(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for _, hash := range []string{"failed-a", "failed-b", "failed-webp", "pending-row", "done-row"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 1, ContentType: "image/jpeg"}); err != nil {
			t.Fatalf("upsert %s: %v", hash, err)
		}
	}
	// Seeded straight in the database, the same way TestResetStuckEncodings
	// builds its fixture: the skip path is exercised by its own tests, and a
	// failed-looking WebP row is exactly the shape no legitimate path produces
	// today — the reset's guard exists for it.
	seed := []struct {
		hash       string
		status     string
		token      string
		stageKey   string
		lease      int64
		sourceSize int64
		errorText  string
	}{
		{hash: "failed-a", status: "failed", token: "tok-a", stageKey: "encoding/failed-a_tok-a.webp", lease: 111, errorText: "encoder crash"},
		{hash: "failed-b", status: "failed", token: "tok-b", errorText: "download truncated"},
		{hash: "failed-webp", status: "failed", token: "tok-webp", sourceSize: 999, errorText: "legacy status"},
		{hash: "pending-row", status: "pending"},
		{hash: "done-row", status: "done", sourceSize: 100},
	}
	for _, item := range seed {
		if _, err := store.db.ExecContext(ctx, `
			UPDATE blobs SET encoding_status=?, encoding_token=?, encoding_stage_key=?, encoding_lease_until=?, encoding_source_size_bytes=?, encoding_error=? WHERE hash=?`,
			item.status, item.token, item.stageKey, item.lease, item.sourceSize, item.errorText, item.hash); err != nil {
			t.Fatalf("seed %s: %v", item.hash, err)
		}
	}
	// The WebP guard is about the row's content type, so it is flipped last,
	// after the shared seeding above.
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET content_type='image/webp' WHERE hash='failed-webp'`); err != nil {
		t.Fatalf("flip failed-webp content type: %v", err)
	}

	// A hash scope moves only the listed failed row; failed-b stays failed.
	reset, err := store.ResetFailedEncodings(ctx, []string{"failed-a"})
	if err != nil {
		t.Fatalf("scoped reset failed encodings: %v", err)
	}
	if reset != 1 {
		t.Fatalf("scoped reset=%d want=1 (only the listed hash)", reset)
	}
	status, token, stageKey, lease, sourceSize := encodingRawState(t, store, "failed-a")
	if status != "pending" || token != "" || stageKey != "" || lease != 0 || sourceSize != 0 {
		t.Fatalf("failed-a=(%s %q %q %d %d) want pending with cleared token/stage/lease/source size", status, token, stageKey, lease, sourceSize)
	}
	if errText := encodingErrorText(t, store, "failed-a"); errText != "" {
		t.Fatalf("failed-a error text=%q want cleared", errText)
	}

	// An empty scope retries every failed non-WebP row: failed-b moves, while
	// the WebP row keeps its legacy status and bookkeeping.
	reset, err = store.ResetFailedEncodings(ctx, nil)
	if err != nil {
		t.Fatalf("reset failed encodings: %v", err)
	}
	if reset != 1 {
		t.Fatalf("reset=%d want=1 (failed-b only; the WebP row is excluded)", reset)
	}
	status, token, stageKey, lease, sourceSize = encodingRawState(t, store, "failed-b")
	if status != "pending" || token != "" || stageKey != "" || lease != 0 || sourceSize != 0 {
		t.Fatalf("failed-b=(%s %q %q %d %d) want pending with cleared token/stage/lease/source size", status, token, stageKey, lease, sourceSize)
	}
	if errText := encodingErrorText(t, store, "failed-b"); errText != "" {
		t.Fatalf("failed-b error text=%q want cleared", errText)
	}
	status, token, stageKey, lease, sourceSize = encodingRawState(t, store, "failed-webp")
	if status != "failed" || token != "tok-webp" || lease != 0 || sourceSize != 999 {
		t.Fatalf("failed-webp=(%s %q %q %d %d) want untouched failed-looking WebP row", status, token, stageKey, lease, sourceSize)
	}
	if errText := encodingErrorText(t, store, "failed-webp"); errText != "legacy status" {
		t.Fatalf("failed-webp error text=%q want untouched", errText)
	}

	// Rows that are not failed never move, and a second retry is a no-op.
	status, _, _, _, sourceSize = encodingRawState(t, store, "pending-row")
	if status != "pending" || sourceSize != 0 {
		t.Fatalf("pending-row=(%s %d) want untouched pending row", status, sourceSize)
	}
	status, _, _, _, sourceSize = encodingRawState(t, store, "done-row")
	if status != "done" || sourceSize != 100 {
		t.Fatalf("done-row=(%s %d) want untouched done row", status, sourceSize)
	}
	reset, err = store.ResetFailedEncodings(ctx, nil)
	if err != nil || reset != 0 {
		t.Fatalf("second reset=%d err=%v want 0", reset, err)
	}
}
