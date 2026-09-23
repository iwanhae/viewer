// Package qdrant is a minimal REST client for the part of the Qdrant vector
// database that the viewer uses to store and search photo embeddings. It
// speaks JSON only — this Qdrant build rejects base64-encoded vectors — and
// expects the server to sit behind a reverse proxy that authenticates the
// "api-key" header.
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// requestTimeout bounds a single Qdrant call: the server is remote and a
	// hung request must not tie up a viewer request forever.
	requestTimeout = 30 * time.Second

	// batchSize keeps each upsert or scroll request body modest so a failed
	// chunk is cheap to retry and stays well under proxy payload limits.
	batchSize = 256

	// maxErrBody caps how much of an error response body lands in an error
	// message: enough to see Qdrant's explanation, not enough to flood logs.
	maxErrBody = 512
)

// pointNamespace is the fixed UUIDv5 namespace every point ID derives from.
// It is computed once at startup; PointID output must stay stable forever,
// otherwise re-upserts would duplicate points instead of overwriting them.
var pointNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("viewer/photo-embeddings"))

// PointID returns the deterministic Qdrant point ID for one photo (albumID,
// idx). The same photo always maps to the same ID, so re-upserting an album
// overwrites its points in place rather than duplicating them.
func PointID(albumID string, idx int) string {
	return uuid.NewSHA1(pointNamespace, []byte(albumID+"/"+strconv.Itoa(idx))).String()
}

// PhotoRecord is one photo point to store: the photo (album, idx) with its
// blob's embedding vector copied in.
type PhotoRecord struct {
	AlbumID string
	Idx     int
	Hash    string
	W, H    int
	Vector  []float32
}

// PhotoHit is one flattened search result: the stored photo plus its cosine
// similarity score (higher is more similar; 1.0 for an identical vector).
type PhotoHit struct {
	AlbumID string
	Idx     int
	Hash    string
	W, H    int
	Score   float64
}

// Client talks to one Qdrant collection over REST. It is safe for concurrent
// use.
type Client struct {
	http       *http.Client
	base       *url.URL
	apiKey     string
	collection string
	dim        int
}

// New builds a client for the collection named collection, whose vectors have
// dim dimensions. baseURL is the Qdrant root (any trailing slash is
// tolerated); apiKey may be empty when the proxy does not require one.
func New(baseURL, apiKey, collection string, dim int) *Client {
	trimmed := strings.TrimRight(baseURL, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		// The base URL comes from configuration, not user input. Rather than
		// panic in a constructor, keep the raw value so the first request
		// fails loudly with an unsupported-protocol error instead.
		u = &url.URL{Fragment: trimmed}
	}
	return &Client{
		http:       &http.Client{Timeout: requestTimeout},
		base:       u,
		apiKey:     apiKey,
		collection: collection,
		dim:        dim,
	}
}

// endpoint joins path under the client's collection into an absolute URL,
// applying query parameters when present.
func (c *Client) endpoint(path string, query url.Values) string {
	u := *c.base
	u.Path += "/collections/" + url.PathEscape(c.collection) + path
	u.RawQuery = query.Encode()
	return u.String()
}

// do sends one request and decodes a 2xx JSON response into out (nil skips
// decoding). Non-2xx responses become an *APIError, which callers unwrap when
// a specific status carries meaning.
func (c *Client) do(ctx context.Context, method, endpoint string, body, out any) error {
	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s request: %w", method, endpoint, err)
		}
		payload = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return fmt.Errorf("build %s %s request: %w", method, endpoint, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The proxy in front of Qdrant expects the key under exactly this header;
	// with no configured key we send nothing so local dev can stay keyless.
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s %s response: %w", method, endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			Method:     method,
			URL:        endpoint,
			Status:     resp.StatusCode,
			StatusText: resp.Status,
			Body:       bodySnippet(data),
		}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, endpoint, err)
		}
	}
	return nil
}

// bodySnippet caps an error body at maxErrBody bytes so log lines and API
// errors stay readable even when a proxy dumps a full HTML error page.
func bodySnippet(b []byte) string {
	if len(b) > maxErrBody {
		return string(b[:maxErrBody])
	}
	return string(b)
}

// APIError is a non-2xx response from Qdrant. Callers unwrap it with
// errors.As when a specific status carries meaning — most importantly a 404
// from a search, which means "no recommendations yet" rather than a failure.
type APIError struct {
	Method     string
	URL        string
	Status     int
	StatusText string
	Body       string
}

// Error renders the request, the status line, and a capped body snippet.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s %s: unexpected status %s", e.Method, e.URL, e.StatusText)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// isStatus reports whether err is or wraps an APIError with that status.
func isStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// EnsureCollection makes the client's collection exist with the vector size
// and distance the embedder produces: an existing collection whose config
// drifted is a hard error, since searching it would silently corrupt results.
func (c *Client) EnsureCollection(ctx context.Context) error {
	var info collectionInfo
	err := c.do(ctx, http.MethodGet, c.endpoint("", nil), nil, &info)
	if err == nil {
		return info.Result.Config.Params.Vectors.checkMatches(c.collection, c.dim)
	}
	if isStatus(err, http.StatusNotFound) {
		if cerr := c.createCollection(ctx); cerr != nil {
			return fmt.Errorf("create collection %q: %w", c.collection, cerr)
		}
		return nil
	}
	return fmt.Errorf("get collection %q: %w", c.collection, err)
}

// createCollection puts a fresh Cosine collection of the client's dimension.
func (c *Client) createCollection(ctx context.Context) error {
	body := createCollectionRequest{Vectors: vectorsConfig{Size: c.dim, Distance: "Cosine"}}
	var out struct {
		Status string `json:"status"`
		OK     bool   `json:"ok"`
	}
	if err := c.do(ctx, http.MethodPut, c.endpoint("", nil), body, &out); err != nil {
		return err
	}
	// Current Qdrant replies {"result":true,"status":"ok"}; older builds set
	// only "ok":true, so either spelling counts as success.
	if out.Status != "ok" && !out.OK {
		return fmt.Errorf("unexpected response status %q", out.Status)
	}
	return nil
}

// UpsertPhotos writes records as Qdrant points, replacing any points these
// photos already have because IDs are deterministic. Every record is
// validated before anything is sent, so one malformed photo cannot leave the
// batch half-written by a later-chunk rejection.
func (c *Client) UpsertPhotos(ctx context.Context, records []PhotoRecord) error {
	for i, r := range records {
		switch {
		case r.AlbumID == "":
			return fmt.Errorf("record %d: album id is empty", i)
		case r.Hash == "":
			return fmt.Errorf("record %d (album %q): hash is empty", i, r.AlbumID)
		case len(r.Vector) != c.dim:
			// Qdrant would reject the whole chunk anyway; catching it here
			// keeps chunks already sent from being only part of the story.
			return fmt.Errorf("record %d (album %q idx %d): vector has %d dimensions, want %d", i, r.AlbumID, r.Idx, len(r.Vector), c.dim)
		}
	}
	for start := 0; start < len(records); start += batchSize {
		end := min(start+batchSize, len(records))
		if err := c.upsertChunk(ctx, records[start:end]); err != nil {
			return fmt.Errorf("upsert records %d..%d: %w", start, end-1, err)
		}
	}
	return nil
}

// upsertChunk writes one batch of points and requires Qdrant to report the
// write completed: callers use wait=true so the very next search sees them.
func (c *Client) upsertChunk(ctx context.Context, records []PhotoRecord) error {
	points := make([]upsertPoint, len(records))
	for i, r := range records {
		points[i] = upsertPoint{
			ID:      PointID(r.AlbumID, r.Idx),
			Vector:  r.Vector,
			Payload: pointPayload{AlbumID: r.AlbumID, Idx: r.Idx, Hash: r.Hash, W: r.W, H: r.H},
		}
	}
	var out writeResult
	if err := c.do(ctx, http.MethodPut, c.endpoint("/points", url.Values{"wait": {"true"}}), upsertRequest{Points: points}, &out); err != nil {
		return err
	}
	return out.checkCompleted("upsert")
}

// GroupSearch recommends photos similar to the one identified by queryPointID,
// excluding the query photo's own album and hash. Results are grouped by
// album so only one photo per album comes back, then flattened in the order
// Qdrant ranked them. A 404 — query photo or collection not indexed, e.g.
// right after a wipe — means "no recommendations yet" and returns no error, a
// contract the wipe-recovery policy relies on.
func (c *Client) GroupSearch(ctx context.Context, queryPointID, excludeAlbumID, excludeHash string, limit int) ([]PhotoHit, error) {
	if limit <= 0 {
		// Nothing is being asked for; skip the round trip rather than have
		// Qdrant reject the request.
		return nil, nil
	}
	body := groupSearchRequest{
		Query: queryPointID,
		Filter: &filter{MustNot: []condition{
			{Key: "album_id", Match: match{Value: excludeAlbumID}},
			{Key: "hash", Match: match{Value: excludeHash}},
		}},
		GroupBy:     "album_id",
		Limit:       limit,
		GroupSize:   1,
		WithPayload: true,
	}
	var out groupSearchResponse
	err := c.do(ctx, http.MethodPost, c.endpoint("/points/query/groups", nil), body, &out)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("group search: %w", err)
	}
	hits := make([]PhotoHit, 0)
	for _, g := range out.Result.Groups {
		// group_size is 1, but flatten whatever arrives, in order, skipping
		// groups that came back without hits.
		for _, h := range g.Hits {
			hits = append(hits, PhotoHit{
				AlbumID: h.Payload.AlbumID,
				Idx:     h.Payload.Idx,
				Hash:    h.Payload.Hash,
				W:       h.Payload.W,
				H:       h.Payload.H,
				Score:   h.Score,
			})
		}
	}
	return hits, nil
}

// SearchByVectorGrouped returns the limit albums whose best-scoring photo is
// nearest to vector, best album first. The vector comes from the query side
// (the text tower or an uploaded image), so it is sent as a plain JSON float
// array — this Qdrant build rejects base64 — and a missing collection (404)
// means "no results yet", the same contract GroupSearch's wipe-recovery policy
// relies on. Qdrant ranks each group by its top-scoring member, so group_size
// 1 yields exactly the best photo per album; unlike GroupSearch there is no
// exclusion, since a query that did not come from a photo has nothing to
// exclude.
func (c *Client) SearchByVectorGrouped(ctx context.Context, vector []float32, limit int) ([]PhotoHit, error) {
	if limit <= 0 {
		// Nothing is being asked for; skip the round trip rather than have
		// Qdrant reject the request.
		return nil, nil
	}
	if len(vector) != c.dim {
		// The collection is a fixed-dimension Cosine index; a query vector of
		// any other width would be rejected server-side anyway.
		return nil, fmt.Errorf("query vector has %d dimensions, want %d", len(vector), c.dim)
	}
	body := groupedVectorSearchRequest{
		Query:       vector,
		GroupBy:     "album_id",
		Limit:       limit,
		GroupSize:   1,
		WithPayload: true,
	}
	var out groupSearchResponse
	err := c.do(ctx, http.MethodPost, c.endpoint("/points/query/groups", nil), body, &out)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("grouped vector search: %w", err)
	}
	hits := make([]PhotoHit, 0)
	for _, g := range out.Result.Groups {
		// group_size is 1, but flatten whatever arrives, in order, skipping
		// groups that came back without hits.
		for _, h := range g.Hits {
			hits = append(hits, PhotoHit{
				AlbumID: h.Payload.AlbumID,
				Idx:     h.Payload.Idx,
				Hash:    h.Payload.Hash,
				W:       h.Payload.W,
				H:       h.Payload.H,
				Score:   h.Score,
			})
		}
	}
	return hits, nil
}

// RetrieveVectorsByHashes fetches the stored vectors for the given blob
// hashes, keyed by hash, for album reload sync. Hashes that are not indexed
// are simply absent from the result, and duplicate inputs are requested once.
func (c *Client) RetrieveVectorsByHashes(ctx context.Context, hashes []string) (map[string][]float32, error) {
	unique := make([]string, 0, len(hashes))
	seen := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		unique = append(unique, h)
	}
	vectors := make(map[string][]float32, len(unique))
	for start := 0; start < len(unique); start += batchSize {
		end := min(start+batchSize, len(unique))
		if err := c.scrollChunk(ctx, unique[start:end], vectors); err != nil {
			return nil, fmt.Errorf("scroll vectors for hashes %d..%d: %w", start, end-1, err)
		}
	}
	return vectors, nil
}

// scrollChunk fetches one batch of hashes with a single should (OR) filter
// and adds whatever came back to vectors, keyed by payload hash.
func (c *Client) scrollChunk(ctx context.Context, hashes []string, vectors map[string][]float32) error {
	should := make([]condition, len(hashes))
	for i, h := range hashes {
		should[i] = condition{Key: "hash", Match: match{Value: h}}
	}
	body := scrollRequest{
		Filter:     &filter{Should: should},
		WithVector: true,
		// The filter matches at most len(hashes) points, so one page of
		// batchSize always covers the whole chunk without paging.
		Limit: batchSize,
	}
	var out scrollResponse
	if err := c.do(ctx, http.MethodPost, c.endpoint("/points/scroll", nil), body, &out); err != nil {
		return err
	}
	for _, p := range out.Result.Points {
		vectors[p.Payload.Hash] = p.Vector
	}
	return nil
}

// DeleteByAlbum removes every point belonging to albumID, waiting for the
// deletion to land so a following Count or re-upload starts from a clean slate.
func (c *Client) DeleteByAlbum(ctx context.Context, albumID string) error {
	body := deleteByFilterRequest{
		Filter: &filter{Must: []condition{{Key: "album_id", Match: match{Value: albumID}}}},
	}
	var out writeResult
	if err := c.do(ctx, http.MethodPost, c.endpoint("/points/delete", url.Values{"wait": {"true"}}), body, &out); err != nil {
		return fmt.Errorf("delete album %q points: %w", albumID, err)
	}
	return out.checkCompleted(fmt.Sprintf("delete album %q points", albumID))
}

// CountVectors returns the exact number of points currently stored in the
// collection, used to notice a wiped collection and refill it.
func (c *Client) CountVectors(ctx context.Context) (int, error) {
	var out struct {
		Result struct {
			Count int `json:"count"`
		} `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, c.endpoint("/points/count", nil), countRequest{Exact: true}, &out); err != nil {
		return 0, fmt.Errorf("count points: %w", err)
	}
	return out.Result.Count, nil
}

// pointPayload mirrors the payload keys stored alongside every vector.
type pointPayload struct {
	AlbumID string `json:"album_id"`
	Idx     int    `json:"idx"`
	Hash    string `json:"hash"`
	W       int    `json:"w"`
	H       int    `json:"h"`
}

// match is Qdrant's exact-value match condition.
type match struct {
	Value any `json:"value"`
}

// condition is one leaf of a Qdrant filter: a payload field key plus a match.
type condition struct {
	Key   string `json:"key"`
	Match match  `json:"match"`
}

// filter is a Qdrant filter; each call sets only the branches it needs.
type filter struct {
	Must    []condition `json:"must,omitempty"`
	Should  []condition `json:"should,omitempty"`
	MustNot []condition `json:"must_not,omitempty"`
}

// writeResult is the reply of the point-write endpoints. With wait=true the
// nested status says "completed" only once the change is applied, not merely
// queued.
type writeResult struct {
	Result struct {
		Status string `json:"status"`
	} `json:"result"`
}

// checkCompleted rejects anything Qdrant queued instead of applied: callers
// wait on writes precisely so the next read observes them.
func (w writeResult) checkCompleted(op string) error {
	if w.Result.Status != "completed" {
		return fmt.Errorf("%s: qdrant reported status %q, want \"completed\"", op, w.Result.Status)
	}
	return nil
}

// upsertRequest is the PUT .../points body.
type upsertRequest struct {
	Points []upsertPoint `json:"points"`
}

// upsertPoint is one point on the wire: a deterministic ID, the embedding as
// a plain JSON float array (base64 is rejected by this Qdrant build), and the
// photo fields as payload.
type upsertPoint struct {
	ID      string       `json:"id"`
	Vector  []float32    `json:"vector"`
	Payload pointPayload `json:"payload"`
}

// groupSearchRequest is the POST .../points/query/groups body.
type groupSearchRequest struct {
	Query       string  `json:"query"`
	Filter      *filter `json:"filter,omitempty"`
	GroupBy     string  `json:"group_by"`
	Limit       int     `json:"limit"`
	GroupSize   int     `json:"group_size"`
	WithPayload bool    `json:"with_payload"`
}

// groupedVectorSearchRequest is the POST .../points/query/groups body for a
// vector query. The query vector is a plain JSON float array because this
// Qdrant build rejects base64. There is no filter field on purpose: the
// vector searches run from outside the catalog, so nothing is excluded.
type groupedVectorSearchRequest struct {
	Query       []float32 `json:"query"`
	GroupBy     string    `json:"group_by"`
	Limit       int       `json:"limit"`
	GroupSize   int       `json:"group_size"`
	WithPayload bool      `json:"with_payload"`
}

// groupSearchResponse is the slice of the query/groups reply the client uses.
type groupSearchResponse struct {
	Result struct {
		Groups []struct {
			Hits []struct {
				Score   float64      `json:"score"`
				Payload pointPayload `json:"payload"`
			} `json:"hits"`
		} `json:"groups"`
	} `json:"result"`
}

// scrollRequest is the POST .../points/scroll body.
type scrollRequest struct {
	Filter     *filter `json:"filter,omitempty"`
	WithVector bool    `json:"with_vector"`
	Limit      int     `json:"limit"`
}

// scrollResponse is the slice of the scroll reply the client uses.
type scrollResponse struct {
	Result struct {
		Points []struct {
			Payload pointPayload `json:"payload"`
			Vector  []float32    `json:"vector"`
		} `json:"points"`
	} `json:"result"`
}

// deleteByFilterRequest is the POST .../points/delete body.
type deleteByFilterRequest struct {
	Filter *filter `json:"filter"`
}

// countRequest is the POST .../points/count body.
type countRequest struct {
	Exact bool `json:"exact"`
}

// collectionInfo is the slice of GET /collections/{name} the client checks.
type collectionInfo struct {
	Result struct {
		Config struct {
			Params struct {
				Vectors vectorsConfig `json:"vectors"`
			} `json:"params"`
		} `json:"config"`
	} `json:"result"`
}

// vectorsConfig is a collection's vector geometry.
type vectorsConfig struct {
	Size     int    `json:"size"`
	Distance string `json:"distance"`
}

// checkMatches verifies the stored geometry still matches what the embedder
// produces; any drift would quietly corrupt search results, so it is a hard
// error rather than something to repair in place.
func (v vectorsConfig) checkMatches(collection string, dim int) error {
	if v.Size != dim {
		return fmt.Errorf("collection %q has vector size %d, want %d", collection, v.Size, dim)
	}
	if v.Distance != "Cosine" {
		return fmt.Errorf("collection %q uses distance %q, want %q", collection, v.Distance, "Cosine")
	}
	return nil
}

// createCollectionRequest is the PUT /collections/{name} body.
type createCollectionRequest struct {
	Vectors vectorsConfig `json:"vectors"`
}
