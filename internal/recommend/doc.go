// Package recommend serves cross-album recommendations from an external
// Qdrant vector store and provides the single claim/write-back path that both
// the in-process embedding worker and external workers drive: the in-process
// worker drains pending blobs on its own, and external workers lease batches
// over the worker API for the initial backfill.
//
// The service holds no vector state. SQLite keeps the bookkeeping — which blob
// is pending, processing, ready, or failed — and the vector store keeps the
// vectors themselves, one point per (album, idx) a ready blob appears under,
// keyed by a deterministic point ID so re-writes overwrite in place.
// ApplyEmbeddingResults writes the two in that order (store first, catalog
// second) so every crash window recovers by re-embedding and re-upserting the
// same points; Recommend is a single grouped search in the store, and
// ReloadAlbum re-syncs one album's points after its photos change.
package recommend
