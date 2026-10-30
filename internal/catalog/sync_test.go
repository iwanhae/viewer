package catalog

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// seedPairLibrary builds a photo library of albumPlan albums (count photos
// each) plus a per-album sprinkle of two hashes shared across every album, so
// a scan sees the same blob referenced by many photos. It returns the exact
// PhotoPairState expected for every (album_id, idx) pair.
func seedPairLibrary(t *testing.T, store *Store, albumPlan []struct {
	albumID string
	count   int
}) map[[2]any]PhotoPairState {
	t.Helper()
	ctx := context.Background()

	const sharedReady = "shared-ready"
	const sharedProcessing = "shared-processing"
	for _, shared := range []struct {
		hash   string
		status EmbeddingStatus
	}{{sharedReady, EmbeddingStatusReady}, {sharedProcessing, EmbeddingStatusProcessing}} {
		if err := store.UpsertBlob(ctx, Blob{Hash: shared.hash, ContentType: "image/jpeg"}); err != nil {
			t.Fatalf("upsert shared blob %s: %v", shared.hash, err)
		}
		seedBlobStatus(t, store, shared.hash, shared.status)
	}

	uniqueStatuses := []EmbeddingStatus{
		EmbeddingStatusPending, EmbeddingStatusProcessing, EmbeddingStatusReady, EmbeddingStatusFailed,
	}
	expected := make(map[[2]any]PhotoPairState)
	for _, plan := range albumPlan {
		if err := store.CreateAlbum(ctx, Album{
			ID:               plan.albumID,
			OriginalFilename: plan.albumID + ".zip",
			SourceKey:        "uploads/" + plan.albumID + ".zip",
		}); err != nil {
			t.Fatalf("create album %s: %v", plan.albumID, err)
		}
		for i := 0; i < plan.count; i++ {
			uniqueHash := fmt.Sprintf("hash-%s-%03d", plan.albumID, i)
			hash, status := uniqueHash, uniqueStatuses[i%len(uniqueStatuses)]
			shared := false
			switch {
			case i == 7:
				hash, status, shared = sharedReady, EmbeddingStatusReady, true
			case i == 13:
				hash, status, shared = sharedProcessing, EmbeddingStatusProcessing, true
			}
			if !shared {
				if err := store.UpsertBlob(ctx, Blob{Hash: uniqueHash, ContentType: "image/jpeg"}); err != nil {
					t.Fatalf("upsert blob %s: %v", uniqueHash, err)
				}
				seedBlobStatus(t, store, uniqueHash, status)
			}
			if err := store.InsertPhoto(ctx, Photo{
				AlbumID: plan.albumID,
				Index:   i,
				Name:    fmt.Sprintf("photo-%03d.jpg", i),
				Hash:    hash,
				Width:   100,
				Height:  80,
				Ratio:   1.25,
			}); err != nil {
				t.Fatalf("insert photo %s:%d: %v", plan.albumID, i, err)
			}
			expected[[2]any{plan.albumID, i}] = PhotoPairState{
				AlbumID:         plan.albumID,
				Idx:             i,
				Hash:            hash,
				EmbeddingStatus: status,
			}
		}
	}
	return expected
}

func TestForEachPhotoPairPagesAllPairsInOrder(t *testing.T) {
	store := openTestStore(t)

	// 3 albums × 200 + 1 album × 150 = 750 photos: more than one full
	// 500-pair page plus a short remainder page, with album boundaries that
	// do not align with the page size.
	plan := []struct {
		albumID string
		count   int
	}{
		{"album-a", 200},
		{"album-b", 200},
		{"album-c", 200},
		{"album-d", 150},
	}
	expected := seedPairLibrary(t, store, plan)

	var seen []PhotoPairState
	if err := store.ForEachPhotoPair(context.Background(), func(pair PhotoPairState) error {
		seen = append(seen, pair)
		return nil
	}); err != nil {
		t.Fatalf("for each photo pair: %v", err)
	}

	if len(seen) != 750 {
		t.Fatalf("saw %d pairs, want 750", len(seen))
	}
	visited := make(map[[2]any]bool, len(seen))
	var lastAlbum string
	var lastIdx int
	for i, pair := range seen {
		key := [2]any{pair.AlbumID, pair.Idx}
		if visited[key] {
			t.Fatalf("pair %v seen more than once (position %d)", key, i)
		}
		visited[key] = true
		if i > 0 && (pair.AlbumID < lastAlbum || (pair.AlbumID == lastAlbum && pair.Idx <= lastIdx)) {
			t.Fatalf("pair #%d {%s %d} violates (album_id, idx) order after {%s %d}", i, pair.AlbumID, pair.Idx, lastAlbum, lastIdx)
		}
		lastAlbum, lastIdx = pair.AlbumID, pair.Idx
		want, ok := expected[key]
		if !ok {
			t.Fatalf("unexpected pair %v not in library", key)
		}
		if pair != want {
			t.Fatalf("pair %v = %+v, want %+v", key, pair, want)
		}
	}
	if len(visited) != len(expected) {
		t.Fatalf("visited %d distinct pairs, want %d", len(visited), len(expected))
	}
}

func TestForEachPhotoPairAbortsOnCallbackError(t *testing.T) {
	store := openTestStore(t)
	expected := seedPairLibrary(t, store, []struct {
		albumID string
		count   int
	}{{"album-a", 3}})
	if len(expected) != 3 {
		t.Fatalf("seeded %d pairs, want 3", len(expected))
	}

	boom := errors.New("stop scanning")
	var calls int
	err := store.ForEachPhotoPair(context.Background(), func(pair PhotoPairState) error {
		calls++
		if calls == 3 {
			return boom
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected the callback error to abort the scan, got nil")
	}
	if err != boom {
		t.Fatalf("scan error = %v, want the exact callback error %v (returned verbatim, not wrapped)", err, boom)
	}
	if calls != 3 {
		t.Fatalf("callback invoked %d times, want 3 (abort at the failing pair)", calls)
	}
}

func TestResetReadyEmbeddingsByHashesResetsOnlyReadyRowsInScope(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	seeded := []struct {
		hash   string
		status EmbeddingStatus
	}{
		{"ready-in-a", EmbeddingStatusReady},
		{"ready-in-b", EmbeddingStatusReady},
		{"ready-out", EmbeddingStatusReady},
		{"processing-in", EmbeddingStatusProcessing},
		{"failed-in", EmbeddingStatusFailed},
		{"pending-in", EmbeddingStatusPending},
	}
	for _, b := range seeded {
		if err := store.UpsertBlob(ctx, Blob{Hash: b.hash, ContentType: "image/jpeg"}); err != nil {
			t.Fatalf("upsert blob %s: %v", b.hash, err)
		}
		seedBlobStatus(t, store, b.hash, b.status)
		// Give every row a stale error and lease so the test can tell a
		// reset row (both cleared) from an untouched one (both kept).
		if _, err := store.db.ExecContext(ctx,
			`UPDATE blobs SET embedding_error='stale', embedding_lease_until=12345 WHERE hash=?`, b.hash); err != nil {
			t.Fatalf("seed error/lease for %s: %v", b.hash, err)
		}
	}

	changed, err := store.ResetReadyEmbeddingsByHashes(ctx, []string{
		"ready-in-a", "ready-in-b", "processing-in", "failed-in", "pending-in", "missing-hash",
	})
	if err != nil {
		t.Fatalf("reset ready embeddings by hashes: %v", err)
	}
	if changed != 2 {
		t.Fatalf("changed = %d, want 2 (only the ready rows in scope)", changed)
	}

	for _, b := range seeded {
		var status, embedErr string
		var lease int64
		if err := store.db.QueryRowContext(ctx,
			`SELECT embedding_status, embedding_error, embedding_lease_until FROM blobs WHERE hash=?`, b.hash,
		).Scan(&status, &embedErr, &lease); err != nil {
			t.Fatalf("read back blob %s: %v", b.hash, err)
		}
		wantReset := b.hash == "ready-in-a" || b.hash == "ready-in-b"
		switch {
		case wantReset:
			if status != string(EmbeddingStatusPending) {
				t.Fatalf("%s status = %s, want pending", b.hash, status)
			}
			if embedErr != "" || lease != 0 {
				t.Fatalf("%s error/lease = %q/%d, want cleared \"\"/0", b.hash, embedErr, lease)
			}
		case b.hash == "ready-out":
			if status != string(EmbeddingStatusReady) || embedErr != "stale" || lease != 12345 {
				t.Fatalf("ready row outside scope %s was touched: status=%s error=%q lease=%d", b.hash, status, embedErr, lease)
			}
		case b.hash == "processing-in":
			if status != string(EmbeddingStatusProcessing) || embedErr != "stale" || lease != 12345 {
				t.Fatalf("processing row %s lost its lease: status=%s error=%q lease=%d", b.hash, status, embedErr, lease)
			}
		case b.hash == "failed-in":
			if status != string(EmbeddingStatusFailed) || embedErr != "stale" || lease != 12345 {
				t.Fatalf("failed row %s was touched: status=%s error=%q lease=%d", b.hash, status, embedErr, lease)
			}
		default: // pending-in
			if status != string(EmbeddingStatusPending) {
				t.Fatalf("%s status = %s, want still pending", b.hash, status)
			}
		}
	}
}

func TestResetReadyEmbeddingsByHashesEmptyScopeResetsNothing(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	hashes := []string{"ready-a", "ready-b", "ready-c"}
	for _, hash := range hashes {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, ContentType: "image/jpeg"}); err != nil {
			t.Fatalf("upsert blob %s: %v", hash, err)
		}
		seedBlobStatus(t, store, hash, EmbeddingStatusReady)
	}

	// nil and []string{} are both machine-computed "nothing left to reset"
	// scopes; neither may fall through to the whole-library semantics of
	// ResetReadyEmbeddings.
	for name, scope := range map[string][]string{"nil slice": nil, "empty slice": {}} {
		changed, err := store.ResetReadyEmbeddingsByHashes(ctx, scope)
		if err != nil {
			t.Fatalf("%s: reset ready embeddings by hashes: %v", name, err)
		}
		if changed != 0 {
			t.Fatalf("%s: changed = %d, want 0", name, changed)
		}
	}

	for _, hash := range hashes {
		var status string
		if err := store.db.QueryRowContext(ctx,
			`SELECT embedding_status FROM blobs WHERE hash=?`, hash).Scan(&status); err != nil {
			t.Fatalf("read back blob %s: %v", hash, err)
		}
		if status != string(EmbeddingStatusReady) {
			t.Fatalf("blob %s status = %s after empty-scope reset, want still ready", hash, status)
		}
	}
}

func TestResetReadyEmbeddingsByHashesRecordsSyncResetEvent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	const hash = "ready-observed"
	if err := store.UpsertBlob(ctx, Blob{Hash: hash, ContentType: "image/jpeg"}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	seedBlobStatus(t, store, hash, EmbeddingStatusReady)

	if _, err := store.ResetReadyEmbeddingsByHashes(ctx, []string{hash}); err != nil {
		t.Fatalf("reset ready embeddings by hashes: %v", err)
	}

	rows, err := store.db.QueryContext(ctx,
		`SELECT event, status FROM pipeline_events WHERE blob_hash=? AND stage='embedding'`, hash)
	if err != nil {
		t.Fatalf("query pipeline events: %v", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var event, status string
		if err := rows.Scan(&event, &status); err != nil {
			t.Fatalf("scan pipeline event: %v", err)
		}
		if event == "sync_reset" && status == string(EmbeddingStatusPending) {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pipeline events: %v", err)
	}
	if !found {
		t.Fatal("no embedding event with event=sync_reset status=pending in the blob's pipeline history")
	}

	var observed string
	if err := store.db.QueryRowContext(ctx,
		`SELECT status FROM blob_stage_observations WHERE blob_hash=? AND stage='embedding'`, hash,
	).Scan(&observed); err != nil {
		t.Fatalf("read embedding observation: %v", err)
	}
	if observed != string(EmbeddingStatusPending) {
		t.Fatalf("embedding observation status = %s, want pending", observed)
	}
}

func TestResetReadyEmbeddingsByHashesSkipsHashAlreadyFlipped(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	const hash = "already-pending"
	if err := store.UpsertBlob(ctx, Blob{Hash: hash, ContentType: "image/jpeg"}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	seedBlobStatus(t, store, hash, EmbeddingStatusReady)
	// A racing worker claims the blob (ready → processing → pending) before
	// the reconciliation's reset lands: the ready guard must match 0 rows.
	seedBlobStatus(t, store, hash, EmbeddingStatusPending)

	changed, err := store.ResetReadyEmbeddingsByHashes(ctx, []string{hash})
	if err != nil {
		t.Fatalf("reset ready embeddings by hashes: %v", err)
	}
	if changed != 0 {
		t.Fatalf("changed = %d, want 0 for a hash no longer ready", changed)
	}

	var status string
	if err := store.db.QueryRowContext(ctx,
		`SELECT embedding_status FROM blobs WHERE hash=?`, hash).Scan(&status); err != nil {
		t.Fatalf("read back blob: %v", err)
	}
	if status != string(EmbeddingStatusPending) {
		t.Fatalf("blob status = %s, want still pending", status)
	}
}
