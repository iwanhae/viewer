package admin

import (
	"context"
	"fmt"
	"time"

	"viewer/internal/catalog"
)

type OverviewLibraryCounts struct {
	Albums int64 `json:"albums"`
	Photos int64 `json:"photos"`
	Blobs  int64 `json:"blobs"`
}

// EncodingCategories classify each blob by its raw encoder status.
type EncodingCategories struct {
	Queued     catalog.BlobCounts `json:"queued"`
	Processing catalog.BlobCounts `json:"processing"`
	Validating catalog.BlobCounts `json:"validating"`
	Committing catalog.BlobCounts `json:"committing"`
	Completed  catalog.BlobCounts `json:"completed"`
	Skipped    catalog.BlobCounts `json:"skipped"`
	Failed     catalog.BlobCounts `json:"failed"`
	Unknown    catalog.BlobCounts `json:"unknown"`
}

type EncodingOverview struct {
	Categories EncodingCategories `json:"categories"`
}

// EmbeddingCategories classify blobs independently of their encoding status.
type EmbeddingCategories struct {
	Queued     catalog.BlobCounts `json:"queued"`
	Processing catalog.BlobCounts `json:"processing"`
	Completed  catalog.BlobCounts `json:"completed"`
	Failed     catalog.BlobCounts `json:"failed"`
	Unknown    catalog.BlobCounts `json:"unknown"`
}

type EmbeddingOverview struct {
	Categories EmbeddingCategories `json:"categories"`
}

// OverviewVectorCount is the vector-store half of the health section's count
// comparison. Status is "disabled" (no vector store wired), "available"
// (counted), or "unavailable" (the count failed — the overview itself still
// succeeds, with a sanitized message). Agreement compares point counts only;
// it is not a claim of full synchronization or point-by-point integrity.
type OverviewVectorCount struct {
	Status         string  `json:"status"`
	MeasuredAt     *string `json:"measuredAt"`
	ExpectedPoints int64   `json:"expectedPoints"`
	ActualPoints   *int64  `json:"actualPoints"`
	CountAgreement *bool   `json:"countAgreement"`
	Message        string  `json:"message,omitempty"`
}

// OverviewHealth carries the five catalog health numbers (expired leases,
// integrity pair, expected vector points) plus the feature wiring the overview
// page renders as its "시스템 상태" panel. The flags report configuration —
// WORKER_TOKEN, a loaded model, a wired vector store — not worker liveness.
type OverviewHealth struct {
	EncodingExpiredLeases  int64               `json:"encodingExpiredLeases"`
	EmbeddingExpiredLeases int64               `json:"embeddingExpiredLeases"`
	UnreferencedBlobs      int64               `json:"unreferencedBlobs"`
	MissingBlobPhotos      int64               `json:"missingBlobPhotos"`
	EncodingEnabled        bool                `json:"encodingEnabled"`
	ModelEnabled           bool                `json:"modelEnabled"`
	VectorStoreEnabled     bool                `json:"vectorStoreEnabled"`
	VectorCount            OverviewVectorCount `json:"vectorCount"`
}

// Overview is the dashboard's landing snapshot: one consistent catalog read —
// library totals, unique-blob pipeline categories, health counts — plus
// best-effort model and vector-store checks. measuredAt is the UTC
// RFC3339Nano time recorded after the catalog snapshot completes.
type Overview struct {
	MeasuredAt string                `json:"measuredAt"`
	Library    OverviewLibraryCounts `json:"library"`
	Encoding   EncodingOverview      `json:"encoding"`
	Embedding  EmbeddingOverview     `json:"embedding"`
	Health     OverviewHealth        `json:"health"`
}

// Overview returns the catalog snapshot with its health section filled in.
// The catalog numbers come from one short read transaction; the model and
// vector-store checks that follow degrade gracefully — an unavailable Qdrant
// or a failed model load shows up in Health instead of failing the request,
// so the dashboard's landing page stays renderable during outages.
func (s *Service) Overview(ctx context.Context) (Overview, error) {
	if s == nil || s.cat == nil {
		return Overview{}, fmt.Errorf("catalog is not available")
	}

	snapshot, err := s.cat.AdminOverview(ctx)
	if err != nil {
		return Overview{}, err
	}
	measuredAt := time.Now().UTC().Format(time.RFC3339Nano)
	return Overview{
		MeasuredAt: measuredAt,
		Library:    OverviewLibraryCounts{Albums: snapshot.Albums, Photos: snapshot.Photos, Blobs: snapshot.Blobs},
		Encoding: EncodingOverview{
			Categories: EncodingCategories{
				Queued:     snapshot.EncodingCategories["queued"],
				Processing: snapshot.EncodingCategories["processing"],
				Validating: snapshot.EncodingCategories["validating"],
				Committing: snapshot.EncodingCategories["committing"],
				Completed:  snapshot.EncodingCategories["completed"],
				Skipped:    snapshot.EncodingCategories["skipped"],
				Failed:     snapshot.EncodingCategories["failed"],
				Unknown:    snapshot.EncodingCategories["unknown"],
			},
		},
		Embedding: EmbeddingOverview{
			Categories: EmbeddingCategories{
				Queued:     snapshot.EmbeddingCategories["queued"],
				Processing: snapshot.EmbeddingCategories["processing"],
				Completed:  snapshot.EmbeddingCategories["completed"],
				Failed:     snapshot.EmbeddingCategories["failed"],
				Unknown:    snapshot.EmbeddingCategories["unknown"],
			},
		},
		Health: s.overviewHealth(ctx, snapshot),
	}, nil
}

// overviewHealth fills the health section from the catalog snapshot plus the
// process's feature wiring. The vector-count contract: no vector store wired
// means "disabled" with unmeasured (null) fields; a failing CountVectors means
// "unavailable" with a sanitized message; only a successful count reports
// actual points and agreement. None of these states is an error.
func (s *Service) overviewHealth(ctx context.Context, snapshot catalog.AdminOverviewSnapshot) OverviewHealth {
	health := OverviewHealth{
		EncodingExpiredLeases:  snapshot.EncodingExpiredLeases,
		EmbeddingExpiredLeases: snapshot.EmbeddingExpiredLeases,
		UnreferencedBlobs:      snapshot.UnreferencedBlobs,
		MissingBlobPhotos:      snapshot.MissingBlobPhotos,
		EncodingEnabled:        s.encodingEnabled,
		ModelEnabled:           s.recommend != nil && s.recommend.Enabled(),
		VectorCount:            OverviewVectorCount{ExpectedPoints: snapshot.ExpectedVectorPoints},
	}
	if s.vectors == nil {
		health.VectorCount.Status = "disabled"
		return health
	}
	health.VectorStoreEnabled = true
	vectorMeasuredAt := time.Now().UTC().Format(time.RFC3339Nano)
	points, err := s.vectors.CountVectors(ctx)
	if err != nil {
		health.VectorCount.Status = "unavailable"
		health.VectorCount.Message = "Vector-store count is temporarily unavailable; the catalog measurements are still current."
		return health
	}
	actual := int64(points)
	agrees := actual == snapshot.ExpectedVectorPoints
	health.VectorCount.Status = "available"
	health.VectorCount.MeasuredAt = &vectorMeasuredAt
	health.VectorCount.ActualPoints = &actual
	health.VectorCount.CountAgreement = &agrees
	return health
}
