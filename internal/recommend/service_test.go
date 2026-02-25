package recommend

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"viewer/internal/models"
)

func TestApplyAlbumIndexReplacesOnlyTargetAlbumRecords(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
		},
		photoIDsByAlbum: map[string]map[string]struct{}{
			"album-a": {
				imageID("album-a", 0): {},
				imageID("album-a", 1): {},
			},
			"album-b": {
				imageID("album-b", 0): {},
			},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), Vector: []float32{1, 0}},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), Vector: []float32{0, 1}},
		},
		failedByID: map[string]string{
			imageID("album-a", 1): "old failure",
		},
		missingByAlbum: make(map[string]map[int]struct{}),
	}

	s.applyAlbumIndex(models.AlbumIndex{
		AlbumID: "album-a",
		Photos: []models.PhotoMeta{
			{I: 2, Name: "new.jpg", W: 100, H: 100, Ratio: 1},
		},
		Embeddings: map[string]models.PhotoEmbedding{
			"2": {
				Status: embeddingStatusReady,
				Vector: []float32{1, 1},
			},
		},
	})

	if _, ok := s.photosByID[imageID("album-a", 0)]; ok {
		t.Fatalf("expected old album-a photo 0 to be removed")
	}
	if _, ok := s.photosByID[imageID("album-a", 1)]; ok {
		t.Fatalf("expected old album-a photo 1 to be removed")
	}
	if _, ok := s.failedByID[imageID("album-a", 1)]; ok {
		t.Fatalf("expected stale failed marker to be removed")
	}
	if _, ok := s.photosByID[imageID("album-a", 2)]; !ok {
		t.Fatalf("expected new album-a photo to be present")
	}
	if _, ok := s.photosByID[imageID("album-b", 0)]; !ok {
		t.Fatalf("expected album-b records to remain")
	}
	if _, ok := s.photoIDsByAlbum["album-a"][imageID("album-a", 2)]; !ok {
		t.Fatalf("expected reverse index to include new album-a photo")
	}
	if _, ok := s.photoIDsByAlbum["album-a"][imageID("album-a", 0)]; ok {
		t.Fatalf("expected reverse index to drop old album-a photo")
	}
}

func TestClaimRandomMissingAlbumClaimsWholeAlbum(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
		},
		missingByAlbum: map[string]map[int]struct{}{
			"album-a": {
				0: {},
				1: {},
			},
			"album-b": {
				0: {},
			},
		},
		rng: rand.New(rand.NewSource(1)),
	}

	initialMissing := map[string]int{
		"album-a": len(s.missingByAlbum["album-a"]),
		"album-b": len(s.missingByAlbum["album-b"]),
	}

	albumID, photos, ok := s.claimRandomMissingAlbum()
	if !ok {
		t.Fatalf("expected album claim")
	}
	if albumID == "" {
		t.Fatalf("expected claimed album id")
	}
	if len(photos) != initialMissing[albumID] {
		t.Fatalf("expected %d claimed photos for %s, got %d", initialMissing[albumID], albumID, len(photos))
	}
	for _, photo := range photos {
		if photo.AlbumID != albumID {
			t.Fatalf("claimed mixed album photos, got %s want %s", photo.AlbumID, albumID)
		}
	}
	if len(s.missingByAlbum[albumID]) != 0 {
		t.Fatalf("expected claimed album missing set to be empty")
	}
	if _, ok := s.albumMissingPos[albumID]; ok {
		t.Fatalf("expected claimed album to be removed from active missing index")
	}

	otherAlbum := "album-a"
	if albumID == "album-a" {
		otherAlbum = "album-b"
	}
	if len(s.missingByAlbum[otherAlbum]) != initialMissing[otherAlbum] {
		t.Fatalf("expected unclaimed album missing count to stay %d, got %d", initialMissing[otherAlbum], len(s.missingByAlbum[otherAlbum]))
	}
}

func TestClaimRandomMissingAlbumSkipsStalePhotoRecords(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{},
		missingByAlbum: map[string]map[int]struct{}{
			"album-a": {
				0: {},
			},
		},
		rng: rand.New(rand.NewSource(1)),
	}

	albumID, photos, ok := s.claimRandomMissingAlbum()
	if ok {
		t.Fatalf("expected no claim, got album=%s photos=%d", albumID, len(photos))
	}
	if len(s.missingByAlbum["album-a"]) != 0 {
		t.Fatalf("expected stale missing entries to be cleared after claim attempt")
	}
}

func TestRequeueMissingPhotosRestoresPendingSet(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
		},
		failedByID: map[string]string{
			imageID("album-a", 0): "old failure",
			imageID("album-a", 1): "old failure",
		},
		missingByAlbum: map[string]map[int]struct{}{
			"album-a": {},
		},
	}

	s.requeueMissingPhotos("album-a", []int{0, 1, 999})

	if _, ok := s.missingByAlbum["album-a"][0]; !ok {
		t.Fatalf("expected photo 0 to be requeued")
	}
	if _, ok := s.missingByAlbum["album-a"][1]; !ok {
		t.Fatalf("expected photo 1 to be requeued")
	}
	if _, ok := s.missingByAlbum["album-a"][999]; ok {
		t.Fatalf("did not expect unknown photo to be requeued")
	}
	if _, ok := s.failedByID[imageID("album-a", 0)]; ok {
		t.Fatalf("expected stale failure marker for photo 0 to be removed")
	}
	if _, ok := s.failedByID[imageID("album-a", 1)]; ok {
		t.Fatalf("expected stale failure marker for photo 1 to be removed")
	}
	if _, ok := s.albumMissingPos["album-a"]; !ok {
		t.Fatalf("expected album to be present in active missing index")
	}
}

func TestApplyAlbumIndexPreservesProcessFailedAsFailed(t *testing.T) {
	id := imageID("album-a", 0)
	s := &Service{
		processFailedByID: map[string]string{
			id: "embed image: timeout",
		},
	}

	s.applyAlbumIndex(models.AlbumIndex{
		AlbumID: "album-a",
		Photos: []models.PhotoMeta{
			{I: 0, Name: "one.jpg", W: 100, H: 100, Ratio: 1},
		},
	})

	if got, ok := s.failedByID[id]; !ok || got != "embed image: timeout" {
		t.Fatalf("expected process-local failed marker to remain visible, got=%q ok=%v", got, ok)
	}
	if _, ok := s.missingByAlbum["album-a"][0]; ok {
		t.Fatalf("expected process-local failed photo to be excluded from missing set")
	}
	if _, ok := s.albumMissingPos["album-a"]; ok {
		t.Fatalf("did not expect album with only process-local failures to stay in active missing index")
	}
}

func TestApplyAlbumIndexReadyEmbeddingClearsProcessFailed(t *testing.T) {
	id := imageID("album-a", 0)
	s := &Service{
		processFailedByID: map[string]string{
			id: "embed image: timeout",
		},
		failedByID: map[string]string{
			id: "embed image: timeout",
		},
	}

	s.applyAlbumIndex(models.AlbumIndex{
		AlbumID: "album-a",
		Photos: []models.PhotoMeta{
			{I: 0, Name: "one.jpg", W: 100, H: 100, Ratio: 1},
		},
		Embeddings: map[string]models.PhotoEmbedding{
			"0": {
				Status: embeddingStatusReady,
				Vector: []float32{1, 2},
			},
		},
	})

	if _, ok := s.processFailedByID[id]; ok {
		t.Fatalf("expected process-local failed marker to be cleared after ready embedding")
	}
	if _, ok := s.failedByID[id]; ok {
		t.Fatalf("did not expect failed marker after ready embedding")
	}
	if _, ok := s.embeddingsByID[id]; !ok {
		t.Fatalf("expected ready embedding to be tracked")
	}
}

func TestRequeueMissingPhotosSkipsProcessFailed(t *testing.T) {
	failedID := imageID("album-a", 0)
	s := &Service{
		photosByID: map[string]PhotoRecord{
			failedID:              {ImageID: failedID, AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
		},
		processFailedByID: map[string]string{
			failedID: "embed image: timeout",
		},
		missingByAlbum: map[string]map[int]struct{}{
			"album-a": {},
		},
	}

	s.requeueMissingPhotos("album-a", []int{0, 1})

	if _, ok := s.missingByAlbum["album-a"][0]; ok {
		t.Fatalf("did not expect process-failed photo to be requeued")
	}
	if _, ok := s.missingByAlbum["album-a"][1]; !ok {
		t.Fatalf("expected non-failed photo to be requeued")
	}
	if got, ok := s.failedByID[failedID]; !ok || got != "embed image: timeout" {
		t.Fatalf("expected failed marker to remain for process-failed photo, got=%q ok=%v", got, ok)
	}
}

func TestMarkFailedLocalTracksProcessFailure(t *testing.T) {
	id := imageID("album-a", 0)
	s := &Service{
		missingByAlbum: map[string]map[int]struct{}{
			"album-a": {
				0: {},
			},
		},
	}

	s.markFailedLocal(id, "embed image: timeout")

	if got, ok := s.processFailedByID[id]; !ok || got != "embed image: timeout" {
		t.Fatalf("expected process-local failure marker, got=%q ok=%v", got, ok)
	}
	if got, ok := s.failedByID[id]; !ok || got != "embed image: timeout" {
		t.Fatalf("expected failed marker, got=%q ok=%v", got, ok)
	}
	if _, ok := s.missingByAlbum["album-a"][0]; ok {
		t.Fatalf("expected failed photo to be removed from missing set")
	}
	if _, ok := s.albumMissingPos["album-a"]; ok {
		t.Fatalf("did not expect album with no missing photos to remain active")
	}
}

func TestRecommendExcludesSameAlbum(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
			imageID("album-c", 0): {ImageID: imageID("album-c", 0), AlbumID: "album-c", PhotoIndex: 0},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), Vector: []float32{1, 0}},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), Vector: []float32{1, 0}},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), Vector: []float32{0.95, 0.05}},
			imageID("album-c", 0): {ImageID: imageID("album-c", 0), Vector: []float32{0.7, 0.3}},
		},
		failedByID: make(map[string]string),
	}

	resp, err := s.Recommend(context.Background(), "album-a", 0, 3)
	if err != nil {
		t.Fatalf("recommend failed: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 cross-album items, got %d", len(resp.Items))
	}
	for _, item := range resp.Items {
		if item.AlbumID == "album-a" {
			t.Fatalf("same-album recommendation leaked: %+v", item)
		}
	}
	if resp.Items[0].AlbumID != "album-b" || resp.Items[1].AlbumID != "album-c" {
		t.Fatalf("unexpected recommendation order: %+v", resp.Items)
	}
}

func TestRecommendDeduplicatesByTargetAlbum(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
			imageID("album-b", 1): {ImageID: imageID("album-b", 1), AlbumID: "album-b", PhotoIndex: 1},
			imageID("album-c", 0): {ImageID: imageID("album-c", 0), AlbumID: "album-c", PhotoIndex: 0},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), Vector: []float32{1, 0}},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), Vector: []float32{0.99, 0.01}},
			imageID("album-b", 1): {ImageID: imageID("album-b", 1), Vector: []float32{0.98, 0.02}},
			imageID("album-c", 0): {ImageID: imageID("album-c", 0), Vector: []float32{0.9, 0.1}},
		},
		failedByID: make(map[string]string),
	}

	resp, err := s.Recommend(context.Background(), "album-a", 0, 2)
	if err != nil {
		t.Fatalf("recommend failed: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 deduplicated items, got %d", len(resp.Items))
	}
	if resp.Items[0].AlbumID != "album-b" || resp.Items[0].I != 0 {
		t.Fatalf("expected highest-ranked item from album-b first, got %+v", resp.Items[0])
	}
	if resp.Items[1].AlbumID != "album-c" {
		t.Fatalf("expected second item from album-c, got %+v", resp.Items[1])
	}
}

func TestRecommendReturnsEmptyWhenOnlySameAlbumCandidates(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), Vector: []float32{1, 0}},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), Vector: []float32{0.9, 0.1}},
		},
		failedByID: make(map[string]string),
	}

	resp, err := s.Recommend(context.Background(), "album-a", 0, 12)
	if err != nil {
		t.Fatalf("recommend failed: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations, got %d", len(resp.Items))
	}
}

func TestRecommendReturnsEmptyWhenQueryEmbeddingMissing(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), Vector: []float32{1, 0}},
		},
		failedByID: make(map[string]string),
	}

	resp, err := s.Recommend(context.Background(), "album-a", 0, 12)
	if err != nil {
		t.Fatalf("recommend failed: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations, got %d", len(resp.Items))
	}
}

func TestRecommendReturnsEmptyWhenQueryEmbeddingFailed(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
		},
		embeddingsByID: make(map[string]EmbeddingRecord),
		failedByID: map[string]string{
			imageID("album-a", 0): "embed failed",
		},
	}

	resp, err := s.Recommend(context.Background(), "album-a", 0, 12)
	if err != nil {
		t.Fatalf("recommend failed: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations, got %d", len(resp.Items))
	}
}

func TestEmbeddingProgressEmptyService(t *testing.T) {
	var svc *Service
	got := svc.EmbeddingProgress()
	if got.Total != 0 || got.Ready != 0 || got.Failed != 0 || got.Pending != 0 || got.Processed != 0 {
		t.Fatalf("expected zero progress for nil service, got %+v", got)
	}
	if got.Ratio != 0 || got.Percent != 0 {
		t.Fatalf("expected zero progress ratio for nil service, got ratio=%f percent=%f", got.Ratio, got.Percent)
	}
}

func TestEmbeddingProgressCountsAndRatios(t *testing.T) {
	s := &Service{
		photosByID: map[string]PhotoRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), AlbumID: "album-a", PhotoIndex: 0},
			imageID("album-a", 1): {ImageID: imageID("album-a", 1), AlbumID: "album-a", PhotoIndex: 1},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), AlbumID: "album-b", PhotoIndex: 0},
			imageID("album-b", 1): {ImageID: imageID("album-b", 1), AlbumID: "album-b", PhotoIndex: 1},
		},
		embeddingsByID: map[string]EmbeddingRecord{
			imageID("album-a", 0): {ImageID: imageID("album-a", 0), Vector: []float32{1, 0}},
			imageID("album-b", 0): {ImageID: imageID("album-b", 0), Vector: []float32{0, 1}},
		},
		failedByID: map[string]string{
			imageID("album-a", 1): "embed failed",
		},
		missingByAlbum: map[string]map[int]struct{}{
			"album-b": {
				1: {},
			},
		},
	}

	got := s.EmbeddingProgress()
	if got.Total != 4 || got.Ready != 2 || got.Failed != 1 || got.Pending != 1 || got.Processed != 3 {
		t.Fatalf("unexpected embedding progress counts: %+v", got)
	}
	if math.Abs(got.Ratio-0.5) > 1e-9 {
		t.Fatalf("ratio=%f want=0.5", got.Ratio)
	}
	if math.Abs(got.Percent-50) > 1e-9 {
		t.Fatalf("percent=%f want=50", got.Percent)
	}
}
