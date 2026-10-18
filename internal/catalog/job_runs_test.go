package catalog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func createTestRun(t *testing.T, s *Store, stage, key string) JobRun {
	t.Helper()
	r, repeated, err := s.CreateJobRun(context.Background(), "operator", stage, key, "test/v1")
	if err != nil || repeated {
		t.Fatalf("create: %+v %v %v", r, repeated, err)
	}
	return r
}

func nextTestRunItem(t *testing.T, s *Store, id string) JobRunItem {
	t.Helper()
	i, err := s.NextJobRunItem(context.Background(), id)
	if err != nil || i == nil {
		t.Fatalf("next item: %+v %v", i, err)
	}
	return *i
}

func TestJobRunSnapshotIdempotencyAndIndependentStages(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedClaimBlobs(t, s, "pending", "failed", "processing", "ready")
	seedBlobStatus(t, s, "failed", EmbeddingStatusFailed)
	seedBlobStatus(t, s, "processing", EmbeddingStatusProcessing)
	seedBlobStatus(t, s, "ready", EmbeddingStatusReady)
	if err := s.UpsertBlob(ctx, Blob{Hash: "webp", SizeBytes: 10, ContentType: "image/webp"}); err != nil {
		t.Fatal(err)
	}
	// A deduplicated registration cannot create another target.
	if err := s.UpsertBlob(ctx, Blob{Hash: "pending", SizeBytes: 10, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	r := createTestRun(t, s, "embedding", "embed-key")
	if r.Status != "queued" || r.Counts.Total != 2 || r.Counts.Pending != 2 || r.Executor != "test/v1" {
		t.Fatalf("snapshot=%+v", r)
	}
	seedClaimBlobs(t, s, "later")
	replayed, repeated, err := s.CreateJobRun(ctx, "operator", "embedding", "embed-key", "different-executor")
	if err != nil || !repeated || !reflect.DeepEqual(replayed, r) {
		t.Fatalf("idempotency=%+v %v %v", replayed, repeated, err)
	}
	if _, _, err := s.CreateJobRun(ctx, "operator", "encoding", "embed-key", "test/v1"); !errors.Is(err, ErrJobRunConflict) {
		t.Fatalf("key rebound: %v", err)
	}
	if _, _, err := s.CreateJobRun(ctx, "other-actor", "embedding", "embed-key", "test/v1"); !errors.Is(err, ErrJobRunConflict) {
		t.Fatalf("active stage conflict: %v", err)
	}
	enc := createTestRun(t, s, "encoding", "encode-key")
	if enc.Counts.Total != 5 {
		t.Fatalf("encoding independent snapshot=%+v", enc)
	}
	page, err := s.JobRunItems(ctx, r.ID, 1, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Hash != "pending" || page.NextCursor == nil {
		t.Fatalf("first page=%+v %v", page, err)
	}
	page2, err := s.JobRunItems(ctx, r.ID, 1, *page.NextCursor)
	if err != nil || len(page2.Items) != 1 || page2.Items[0].Hash != "webp" || page2.NextCursor != nil {
		t.Fatalf("second page=%+v %v", page2, err)
	}
	if _, err := s.JobRunItems(ctx, enc.ID, 1, *page.NextCursor); !errors.Is(err, ErrInvalidJobRun) {
		t.Fatalf("cross-run cursor: %v", err)
	}
	list, err := s.JobRuns(ctx, 1, "")
	if err != nil || len(list.Runs) != 1 || list.Runs[0].ID != enc.ID || list.NextCursor == nil {
		t.Fatalf("run list=%+v %v", list, err)
	}
	list, err = s.JobRuns(ctx, 1, *list.NextCursor)
	if err != nil || len(list.Runs) != 1 || list.Runs[0].ID != r.ID || list.NextCursor != nil {
		t.Fatalf("run list next=%+v %v", list, err)
	}
}

func TestJobRunConcurrentStageExclusivity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.CreateJobRun(ctx, "actor", "embedding", fmt.Sprint(i), "test/v1")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrJobRunConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("created %d simultaneous embedding runs", winners)
	}
	createTestRun(t, s, "encoding", "encoding")
}

func TestJobRunConcurrentIdempotencyAndCancelBeforeStart(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedClaimBlobs(t, s, "a", "b")
	var wg sync.WaitGroup
	type result struct {
		run      JobRun
		repeated bool
		err      error
	}
	results := make(chan result, 12)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, repeated, err := s.CreateJobRun(ctx, "actor", "encoding", "same-key", "test/v1")
			results <- result{r, repeated, err}
		}()
	}
	wg.Wait()
	close(results)
	id, created := "", 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if id == "" {
			id = result.run.ID
		}
		if result.run.ID != id {
			t.Fatalf("idempotency created another run: %s != %s", result.run.ID, id)
		}
		if !result.repeated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d runs for one key", created)
	}
	if _, err := s.RequestJobRunCancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.StartJobRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if item, err := s.NextJobRunItem(ctx, id); err != nil || item != nil {
		t.Fatalf("cancelled queued run claimed: %+v %v", item, err)
	}
	if err := s.FinishJobRun(ctx, id, "", ""); err != nil {
		t.Fatal(err)
	}
	r, err := s.JobRun(ctx, id)
	if err != nil || r.Status != "cancelled" || r.StartedAt != nil || r.Counts.Cancelled != 2 {
		t.Fatalf("cancel before start=%+v %v", r, err)
	}
}

func TestJobRunCancelPreservesOutcomesAndExternalOwnership(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedClaimBlobs(t, s, "a", "b", "c", "d")
	r := createTestRun(t, s, "embedding", "key")
	if err := s.StartJobRun(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	a := nextTestRunItem(t, s, r.ID)
	if err := s.FinishJobRunItem(ctx, r.ID, a, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	// Terminal item updates are idempotent; stale tokens cannot overwrite them.
	if err := s.FinishJobRunItem(ctx, r.ID, a, "failed", "processing_failed"); err != nil {
		t.Fatal(err)
	}
	b := nextTestRunItem(t, s, r.ID)
	stale := b
	stale.ClaimToken = "stale"
	if err := s.FinishJobRunItem(ctx, r.ID, stale, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	// The worker wins after snapshot. Bookkeeping supports busy without any
	// release or mutation of that worker's blob claim.
	claims, err := s.ClaimExternalEmbeddings(ctx, 4, time.Now().Add(time.Hour))
	if err != nil || len(claims) != 4 {
		t.Fatalf("external claims=%+v %v", claims, err)
	}
	if err := s.FinishJobRunItem(ctx, r.ID, b, "busy", "target_busy"); err != nil {
		t.Fatal(err)
	}
	c := nextTestRunItem(t, s, r.ID)
	first, err := s.RequestJobRunCancel(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RequestJobRunCancel(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("cancel not idempotent: %+v %+v %v", first, second, err)
	}
	if i, err := s.NextJobRunItem(ctx, r.ID); err != nil || i != nil {
		t.Fatalf("claimed after cancel: %+v %v", i, err)
	}
	if err := s.FinishJobRunItem(ctx, r.ID, c, "cancelled", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobRun(ctx, r.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	final, err := s.JobRun(ctx, r.ID)
	if err != nil || final.Status != "cancelled" || final.Counts.Succeeded != 1 || final.Counts.Busy != 1 || final.Counts.Cancelled != 2 || final.StartedAt == nil || final.FinishedAt == nil {
		t.Fatalf("final=%+v %v", final, err)
	}
	counts, err := s.EmbeddingCounts(ctx)
	if err != nil || counts.Processing != 4 {
		t.Fatalf("cancel touched external leases: %+v %v", counts, err)
	}
	createTestRun(t, s, "embedding", "new-key")
	if replay, repeated, err := s.CreateJobRun(ctx, "operator", "embedding", "key", "test/v1"); err != nil || !repeated || replay.ID != r.ID || replay.Status != "cancelled" {
		t.Fatalf("terminal replay=%+v %v %v", replay, repeated, err)
	}
}

func TestJobRunRestartInterruptsOnlyLocalRunClaims(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runs.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	seedClaimBlobs(t, s, "a-local", "b-success", "c-external", "d-pending")
	lease := time.Now().Add(time.Hour).UnixMilli()
	var ids []string
	for _, stage := range []string{"embedding", "encoding"} {
		r := createTestRun(t, s, stage, stage)
		ids = append(ids, r.ID)
		if err := s.StartJobRun(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		local := nextTestRunItem(t, s, r.ID)
		query := `UPDATE blobs SET embedding_status='processing',embedding_owner='local',embedding_token=?,embedding_lease_until=? WHERE hash=?`
		if stage == "encoding" {
			query = `UPDATE blobs SET encoding_status='leased',encoding_owner='local',encoding_token=?,encoding_lease_until=? WHERE hash=?`
		}
		if _, err := s.db.Exec(query, local.ClaimToken, lease, local.Hash); err != nil {
			t.Fatal(err)
		}
		success := nextTestRunItem(t, s, r.ID)
		if err := s.FinishJobRunItem(ctx, r.ID, success, "succeeded", ""); err != nil {
			t.Fatal(err)
		}
		external := nextTestRunItem(t, s, r.ID)
		query = `UPDATE blobs SET embedding_status='processing',embedding_owner='external',embedding_token='',embedding_lease_until=? WHERE hash=?`
		if stage == "encoding" {
			query = `UPDATE blobs SET encoding_status='leased',encoding_token='external-token',encoding_lease_until=? WHERE hash=?`
		}
		if _, err := s.db.Exec(query, lease, external.Hash); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE blobs SET embedding_status='ready',encoding_status='done' WHERE hash='b-success'`); err != nil {
		t.Fatal(err)
	}
	// The external lease survives even if its previous HTTP result request
	// died after reserving a write. Only that server-owned reservation clears.
	if _, err := s.db.Exec(`UPDATE blobs SET embedding_finalizing='dead-server-request' WHERE hash='c-external'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		r, err := s.JobRun(ctx, id)
		if err != nil || r.Status != "interrupted" || r.Counts.Succeeded != 1 || r.Counts.Interrupted != 3 || r.Counts.Pending+r.Counts.Running != 0 || r.LastError != "service_stopped" || r.FinishedAt == nil {
			t.Fatalf("restart=%+v %v", r, err)
		}
		if i, err := s.NextJobRunItem(ctx, id); err != nil || i != nil {
			t.Fatalf("interrupted resumed: %+v %v", i, err)
		}
	}
	var embStatus, owner, encStatus, token string
	var embLease, encLease int64
	if err := s.db.QueryRow(`SELECT embedding_status,embedding_owner,embedding_lease_until,encoding_status,encoding_token,encoding_lease_until FROM blobs WHERE hash='c-external'`).Scan(&embStatus, &owner, &embLease, &encStatus, &token, &encLease); err != nil {
		t.Fatal(err)
	}
	if embStatus != "processing" || owner != "external" || embLease != lease || encStatus != "leased" || token != "external-token" || encLease != lease {
		t.Fatalf("external ownership altered: %s %s %d %s %s %d", embStatus, owner, embLease, encStatus, token, encLease)
	}
	var reservation string
	if err := s.db.QueryRow(`SELECT embedding_finalizing FROM blobs WHERE hash='c-external'`).Scan(&reservation); err != nil || reservation != "" {
		t.Fatalf("dead server request stranded external results: %q %v", reservation, err)
	}
	if err := s.db.QueryRow(`SELECT embedding_status,encoding_status FROM blobs WHERE hash='a-local'`).Scan(&embStatus, &encStatus); err != nil {
		t.Fatal(err)
	}
	if embStatus != "pending" || encStatus != "pending" {
		t.Fatalf("local claims not released: %s %s", embStatus, encStatus)
	}
	if err := s.db.QueryRow(`SELECT embedding_status,encoding_status FROM blobs WHERE hash='b-success'`).Scan(&embStatus, &encStatus); err != nil {
		t.Fatal(err)
	}
	if embStatus != "ready" || encStatus != "done" {
		t.Fatalf("success rolled back: %s %s", embStatus, encStatus)
	}
	// Both stages can be explicitly submitted again, without auto-resuming.
	for _, stage := range []string{"embedding", "encoding"} {
		createTestRun(t, s, stage, "new-"+stage)
	}
}

func TestJobRunQueuedRestartAndSafeErrors(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runs.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedClaimBlobs(t, s, "a")
	queued := createTestRun(t, s, "embedding", "queued")
	failed := createTestRun(t, s, "encoding", "failed")
	if err := s.StartJobRun(ctx, failed.ID); err != nil {
		t.Fatal(err)
	}
	i := nextTestRunItem(t, s, failed.ID)
	if err := s.FinishJobRunItem(ctx, failed.ID, i, "failed", "https://secret.invalid/?token=secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobRun(ctx, failed.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.JobRun(ctx, queued.ID)
	if err != nil || r.Status != "interrupted" || r.Counts.Interrupted != 1 {
		t.Fatalf("queued recovery=%+v %v", r, err)
	}
	page, err := s.JobRunItems(ctx, failed.ID, 10, "")
	if err != nil || page.Run.Status != "failed" || page.Run.LastError != "executor_failed" || page.Items[0].LastError != "executor_failed" {
		t.Fatalf("safe error=%+v %v", page, err)
	}
}

func TestJobRunInterruptionDoesNotRollbackAmbiguousEncodingCommit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedClaimBlobs(t, s, "a")
	r := createTestRun(t, s, "encoding", "key")
	if err := s.StartJobRun(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	i := nextTestRunItem(t, s, r.ID)
	lease := time.Now().Add(time.Hour).UnixMilli()
	if _, err := s.db.Exec(`UPDATE blobs SET encoding_status='committing',encoding_token=?,encoding_lease_until=?,encoding_source_size_bytes=100 WHERE hash=?`, i.ClaimToken, lease, i.Hash); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobRun(ctx, r.ID, "interrupted", "service_stopped"); err != nil {
		t.Fatal(err)
	}
	job, err := s.GetEncodingJob(ctx, i.Hash)
	if err != nil || job.Status != "committing" || job.Token != i.ClaimToken || job.LeaseUntil.UnixMilli() != lease {
		t.Fatalf("ambiguous commit rolled back: %+v %v", job, err)
	}
	var size int64
	if err := s.db.QueryRow(`SELECT encoding_source_size_bytes FROM blobs WHERE hash=?`, i.Hash).Scan(&size); err != nil || size != 100 {
		t.Fatalf("commit evidence lost: %d %v", size, err)
	}
}
