// Package recommend serves cross-album recommendations from an external
// Qdrant vector store and provides a shared queue with separate ownership paths
// for token-fenced in-process workers and hash-only external workers. The local
// worker is hosted by the service itself: Start launches it after a successful
// model load, and the operator's enable/pause switch (SetAutomaticEmbedding)
// gates it between batches. External workers lease batches over the worker API
// independently of that local control.
//
// The service holds no vector state. SQLite keeps the bookkeeping — which blob
// is pending, processing, ready, or failed — and the vector store keeps the
// vectors themselves, one point per (album, idx) a ready blob appears under,
// keyed by a deterministic point ID so re-writes overwrite in place.
// Finalization reserves ownership before vector writes, without holding a
// SQLite transaction across network calls. It writes store first, catalog
// second so every crash window recovers by re-embedding and re-upserting the
// same points; Recommend is a single grouped search in the store, and
// ReloadAlbum re-syncs one album's points after its photos change.
package recommend
