// Package recommend serves cross-album recommendations from the vector index
// the SQLite catalog keeps in its blob_embeddings vec0 table, and provides the
// single claim/write-back path that both the in-process embedding worker and
// external workers drive: the in-process worker drains pending blobs on its
// own, and external workers lease batches over the worker API for the initial
// backfill. Only the photo-to-blob fan-out lives in memory here; embeddings
// themselves are never held outside SQLite.
package recommend
