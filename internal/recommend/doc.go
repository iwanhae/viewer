// Package recommend keeps an in-memory similarity index over the blob
// embeddings stored in the SQLite catalog, serves cross-album recommendations,
// and provides the single claim/write-back path that both the in-process
// embedding worker and external workers drive: the in-process worker drains
// pending blobs on its own, and external workers lease batches over the worker
// API for the initial backfill.
package recommend
