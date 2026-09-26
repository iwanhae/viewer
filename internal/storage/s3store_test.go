package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	cfgpkg "viewer/internal/config"
)

// listObjectsXML is a canned ListObjectsV2 response whose keys already carry
// the store's key prefix, exactly as a real bucket would return them.
const listObjectsXML = `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>viewer</Name>
  <Prefix>viewer/uploads/</Prefix>
  <KeyCount>2</KeyCount>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>viewer/uploads/second.zip</Key>
    <LastModified>2024-01-02T03:04:05.000Z</LastModified>
    <ETag>&quot;etag-2&quot;</ETag>
    <Size>20</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
  <Contents>
    <Key>viewer/uploads/first.zip</Key>
    <LastModified>2024-01-01T00:00:00.000Z</LastModified>
    <ETag>&quot;etag-1&quot;</ETag>
    <Size>10</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
</ListBucketResult>`

type recordedRequest struct {
	method            string
	host              string
	path              string
	query             string
	body              string
	copySource        string
	copyMatch         string
	metadataDirective string
	contentType       string
}

func (r recordedRequest) String() string {
	return r.method + " " + r.host + r.path + "?" + r.query
}

// newRecordingStore builds a real S3 client pointed at a stub object store, so
// the tests assert the requests that actually reach the wire rather than the
// internal key helpers.
func newRecordingStore(t *testing.T, keyPrefix string) (*S3Store, *[]recordedRequest) {
	t.Helper()
	return newRecordingStoreAt(t, "", keyPrefix)
}

// newRecordingStoreAt is newRecordingStore with a path prefix on the endpoint
// itself, the shape a store behind a gateway exposes.
func newRecordingStoreAt(t *testing.T, endpointPath string, keyPrefix string) (*S3Store, *[]recordedRequest) {
	t.Helper()

	requests := make([]recordedRequest, 0, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, recordedRequest{
			method:            r.Method,
			host:              r.Host,
			path:              r.URL.Path,
			query:             r.URL.RawQuery,
			body:              string(body),
			copySource:        r.Header.Get("X-Amz-Copy-Source"),
			copyMatch:         r.Header.Get("X-Amz-Copy-Source-If-Match"),
			metadataDirective: r.Header.Get("X-Amz-Metadata-Directive"),
			contentType:       r.Header.Get("Content-Type"),
		})
		switch {
		case r.URL.Query().Get("list-type") == "2":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, listObjectsXML)
		case r.Header.Get("X-Amz-Copy-Source") != "":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<CopyObjectResult><ETag>"copied"</ETag><LastModified>2024-01-02T03:04:05Z</LastModified></CopyObjectResult>`)
		case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`)
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "11")
			w.Header().Set("ETag", `"etag-1"`)
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Last-Modified", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC).Format(http.TimeFormat))
		default:
			w.Header().Set("ETag", `"etag-1"`)
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "image-bytes")
		}
	}))
	t.Cleanup(server.Close)

	store, err := NewS3Store(context.Background(), cfgpkg.Config{
		S3Endpoint:     server.URL + endpointPath,
		S3Bucket:       "test-bucket",
		S3AccessKey:    "access",
		S3SecretKey:    "secret",
		S3Prefix:       keyPrefix,
		S3UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	return store, &requests
}

func TestCopyObjectIfMatchKeepsPrefixAndReplacesType(t *testing.T) {
	store, requests := newRecordingStore(t, "viewer/")
	if err := store.CopyObjectIfMatch(context.Background(), "encoding/hash_token.webp", "blobs/hash", `"stage-etag"`, "image/webp"); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests=%d", len(*requests))
	}
	r := (*requests)[0]
	if r.method != http.MethodPut || r.path != "/test-bucket/viewer/blobs/hash" {
		t.Fatalf("copy destination: %+v", r)
	}
	if r.copySource != "test-bucket/viewer/encoding/hash_token.webp" || r.copyMatch != `"stage-etag"` || r.metadataDirective != "REPLACE" || r.contentType != "image/webp" {
		t.Fatalf("copy headers: %+v", r)
	}
}

// TestS3StoreKeyPrefixReachesEveryRequest is the whole point of S3_PREFIX: the
// prefix has to be added on every path, so that a deployment's objects live
// entirely under its own namespace.
func TestS3StoreKeyPrefixReachesEveryRequest(t *testing.T) {
	store, requests := newRecordingStore(t, "viewer/")
	ctx := context.Background()

	if err := store.PutObject(ctx, "blobs/deadbeef", strings.NewReader("image-bytes"), "image/png"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	body, contentType, err := store.GetObject(ctx, "blobs/deadbeef")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	payload, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if string(payload) != "image-bytes" {
		t.Errorf("body=%q want image-bytes", payload)
	}
	if contentType != "image/png" {
		t.Errorf("contentType=%q want image/png", contentType)
	}

	exists, size, err := store.HeadObject(ctx, "blobs/deadbeef")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if !exists || size != int64(len("image-bytes")) {
		t.Errorf("exists=%v size=%d want true/%d", exists, size, len("image-bytes"))
	}

	if err := store.DeleteObjects(ctx, []string{"blobs/deadbeef"}); err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}

	// A batch delete does not target the object path: it POSTs the keys in an
	// XML body on the bucket root, and the prefix rides inside the body.
	want := []struct{ method, path string }{
		{http.MethodPut, "/test-bucket/viewer/blobs/deadbeef"},
		{http.MethodGet, "/test-bucket/viewer/blobs/deadbeef"},
		{http.MethodHead, "/test-bucket/viewer/blobs/deadbeef"},
		{http.MethodPost, "/test-bucket"},
	}
	got := *requests
	if len(got) != len(want) {
		t.Fatalf("recorded %d requests (%v), want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].method != w.method || got[i].path != w.path {
			t.Errorf("request %d = %s, want %s %s", i, got[i], w.method, w.path)
		}
	}
	deleteReq := got[len(got)-1]
	query, err := neturl.ParseQuery(deleteReq.query)
	if err != nil || !query.Has("delete") {
		t.Errorf("delete query=%q want a delete parameter", deleteReq.query)
	}
	if !strings.Contains(deleteReq.body, "<Key>viewer/blobs/deadbeef</Key>") {
		t.Errorf("delete body=%q want the prefixed key", deleteReq.body)
	}
}

// TestS3StoreListObjectsStripsKeyPrefix covers the one method that reads keys
// back out of the bucket: the upload scan feeds listed keys straight into the
// other methods, so they must stay logical or the prefix would be applied twice.
func TestS3StoreListObjectsStripsKeyPrefix(t *testing.T) {
	store, requests := newRecordingStore(t, "viewer/")

	objects, err := store.ListObjects(context.Background(), "uploads/")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}

	got := *requests
	if len(got) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(got))
	}
	if got[0].query == "" || !strings.Contains(got[0].query, "prefix=viewer%2Fuploads%2F") {
		t.Errorf("list query=%q want a prefixed prefix=viewer%%2Fuploads%%2F", got[0].query)
	}

	if len(objects) != 2 {
		t.Fatalf("ListObjects returned %d objects, want 2", len(objects))
	}
	// Sorted by logical key: the canned response is deliberately out of order.
	if objects[0].Key != "uploads/first.zip" || objects[1].Key != "uploads/second.zip" {
		t.Errorf("keys=%q,%q want uploads/first.zip,uploads/second.zip", objects[0].Key, objects[1].Key)
	}
	if objects[0].ETag != `"etag-1"` || objects[0].Size != 10 {
		t.Errorf("first object=%+v want etag-1 size 10", objects[0])
	}

	// A listed key must round-trip through the other methods unchanged: the
	// batch delete carries the prefix exactly once, in the body's key.
	if err := store.DeleteObjects(context.Background(), []string{objects[0].Key}); err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	deleteReq := (*requests)[1]
	if deleteReq.method != http.MethodPost || deleteReq.path != "/test-bucket" {
		t.Errorf("delete request = %s, want POST /test-bucket", deleteReq)
	}
	if !strings.Contains(deleteReq.body, "<Key>viewer/uploads/first.zip</Key>") ||
		strings.Contains(deleteReq.body, "viewer/viewer/") {
		t.Errorf("delete body=%q want viewer/uploads/first.zip prefixed exactly once", deleteReq.body)
	}
}

// TestS3StoreWithoutPrefixKeepsFlatLayout pins the default: an unset S3_PREFIX
// must not add a slash or otherwise move existing objects.
func TestS3StoreWithoutPrefixKeepsFlatLayout(t *testing.T) {
	store, requests := newRecordingStore(t, "")

	if err := store.PutObject(context.Background(), "blobs/deadbeef", strings.NewReader("x"), "image/png"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if path := (*requests)[0].path; path != "/test-bucket/blobs/deadbeef" {
		t.Errorf("put path=%q want /test-bucket/blobs/deadbeef", path)
	}
}

// TestS3StorePresignsUnderKeyPrefix checks the presigned upload URL the browser
// PUTs to, which never goes through the store's own client. The signature pins
// nothing beyond the Host header the URL already carries, so the PUT needs no
// extra headers.
func TestS3StorePresignsUnderKeyPrefix(t *testing.T) {
	store, _ := newRecordingStore(t, "team-a/")

	url, err := store.PresignPut(context.Background(), "uploads/album-1.zip", time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if !strings.Contains(url, "/test-bucket/team-a/uploads/album-1.zip") {
		t.Errorf("presigned url=%q want the bucket and key prefix in the path", url)
	}
	parsed, err := neturl.Parse(url)
	if err != nil {
		t.Fatalf("parse presigned url %q: %v", url, err)
	}
	if got := parsed.Query().Get("X-Amz-SignedHeaders"); got != "host" {
		t.Errorf("X-Amz-SignedHeaders=%q want host (a plain PUT must satisfy the signature)", got)
	}
}

// TestS3StoreAddressesRequestsPathStyle pins the addressing the self-hosted
// stores need: the bucket is the first path segment of the request rather than
// a subdomain of the endpoint, and an endpoint that already carries a path keeps
// it in front of the bucket.
func TestS3StoreAddressesRequestsPathStyle(t *testing.T) {
	store, requests := newRecordingStoreAt(t, "/gateway", "")

	if err := store.PutObject(context.Background(), "blobs/deadbeef", strings.NewReader("x"), "image/png"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got := *requests
	if len(got) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(got))
	}
	if want := "/gateway/test-bucket/blobs/deadbeef"; got[0].path != want {
		t.Errorf("path=%q want=%q (bucket in the path, endpoint path preserved)", got[0].path, want)
	}
	if strings.HasPrefix(got[0].host, "test-bucket.") {
		t.Errorf("host=%q puts the bucket in a subdomain; want path-style addressing", got[0].host)
	}
}

// newVirtualHostedStore builds a store with the opt-in addressing mode and no
// stub server: the assertions below read presigned URLs, which resolve the
// endpoint without a request. A stub server is no use here because
// "<bucket>.127.0.0.1" cannot be dialled.
func newVirtualHostedStore(t *testing.T, endpoint string) *S3Store {
	t.Helper()

	store, err := NewS3Store(context.Background(), cfgpkg.Config{
		S3Endpoint:     endpoint,
		S3Bucket:       "test-bucket",
		S3AccessKey:    "access",
		S3SecretKey:    "secret",
		S3UsePathStyle: false,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	return store
}

// TestS3StoreVirtualHostedAddressing covers the opt-in mode for stores that
// expect the bucket as a subdomain of the endpoint.
func TestS3StoreVirtualHostedAddressing(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		wantHost string
		wantPath string
	}{
		{
			name:     "plain endpoint",
			endpoint: "https://s3.example.com",
			wantHost: "test-bucket.s3.example.com",
			wantPath: "/uploads/album-1.zip",
		},
		{
			name:     "endpoint with a path prefix",
			endpoint: "https://s3.example.com/gateway",
			wantHost: "test-bucket.s3.example.com",
			wantPath: "/gateway/uploads/album-1.zip",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newVirtualHostedStore(t, tc.endpoint)

			url, err := store.PresignPut(context.Background(), "uploads/album-1.zip", time.Minute)
			if err != nil {
				t.Fatalf("PresignPut: %v", err)
			}
			parsed, err := neturl.Parse(url)
			if err != nil {
				t.Fatalf("parse presigned url %q: %v", url, err)
			}
			if parsed.Host != tc.wantHost {
				t.Errorf("host=%q want=%q", parsed.Host, tc.wantHost)
			}
			if parsed.Path != tc.wantPath {
				t.Errorf("path=%q want=%q (the bucket must not also appear in the path)", parsed.Path, tc.wantPath)
			}
		})
	}
}

// TestS3StoreStatObjectReturnsListingMetadata checks the metadata the catalog
// backup restore decision needs from a HEAD: last-modified above all, plus the
// size the download is validated against.
func TestS3StoreStatObjectReturnsListingMetadata(t *testing.T) {
	store, requests := newRecordingStore(t, "viewer/")

	obj, ok, err := store.StatObject(context.Background(), "blobs/deadbeef")
	if err != nil {
		t.Fatalf("StatObject: %v", err)
	}
	if !ok {
		t.Fatalf("StatObject reported an existing object missing")
	}
	if obj.Key != "blobs/deadbeef" {
		t.Errorf("key=%q want blobs/deadbeef (logical, not prefixed)", obj.Key)
	}
	if obj.Size != 11 {
		t.Errorf("size=%d want 11", obj.Size)
	}
	if obj.ETag != `"etag-1"` {
		t.Errorf("etag=%q want \"etag-1\"", obj.ETag)
	}
	if want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC); !obj.LastModified.Equal(want) {
		t.Errorf("lastModified=%s want %s", obj.LastModified, want)
	}

	got := *requests
	if len(got) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(got))
	}
	if got[0].method != http.MethodHead || got[0].path != "/test-bucket/viewer/blobs/deadbeef" {
		t.Errorf("request = %s, want HEAD /test-bucket/viewer/blobs/deadbeef", got[0])
	}
}

// TestS3StoreStatObjectReportsMissing pins the not-found contract the restore
// decision relies on: a missing object is (Object{}, false, nil), not an error.
func TestS3StoreStatObjectReportsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	store, err := NewS3Store(context.Background(), cfgpkg.Config{
		S3Endpoint:     server.URL,
		S3Bucket:       "test-bucket",
		S3AccessKey:    "access",
		S3SecretKey:    "secret",
		S3UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}

	obj, ok, err := store.StatObject(context.Background(), "blobs/missing")
	if err != nil {
		t.Fatalf("StatObject on a missing object: %v", err)
	}
	if ok {
		t.Errorf("ok=true want false")
	}
	if obj != (Object{}) {
		t.Errorf("obj=%+v want the zero Object", obj)
	}
}

// TestS3StoreDeleteObjectsBatchesAndPrefixes pins the wire shape of the batch
// delete: POSTs on the bucket root whose keys carry the store's prefix, chunked
// at the API's 1000-key limit.
func TestS3StoreDeleteObjectsBatchesAndPrefixes(t *testing.T) {
	store, requests := newRecordingStore(t, "viewer/")

	keys := make([]string, 0, 1001)
	for i := 0; i < 1001; i++ {
		keys = append(keys, fmt.Sprintf("uploads/album-%d.zip", i))
	}
	if err := store.DeleteObjects(context.Background(), keys); err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}

	got := *requests
	if len(got) != 2 {
		t.Fatalf("recorded %d requests, want 2 (one per 1000-key batch)", len(got))
	}
	for i, req := range got {
		if req.method != http.MethodPost {
			t.Errorf("request %d method=%q want POST", i, req.method)
		}
		if req.path != "/test-bucket" {
			t.Errorf("request %d path=%q want /test-bucket (a batch delete targets the bucket root)", i, req.path)
		}
		query, err := neturl.ParseQuery(req.query)
		if err != nil || !query.Has("delete") {
			t.Errorf("request %d query=%q want a delete parameter", i, req.query)
		}
	}
	if !strings.Contains(got[0].body, "<Key>viewer/uploads/album-0.zip</Key>") ||
		!strings.Contains(got[0].body, "<Key>viewer/uploads/album-999.zip</Key>") ||
		strings.Contains(got[0].body, "album-1000.zip") {
		t.Errorf("first batch body=%q want keys 0..999 under the store prefix", got[0].body)
	}
	if !strings.Contains(got[1].body, "<Key>viewer/uploads/album-1000.zip</Key>") ||
		strings.Contains(got[1].body, "album-0.zip") {
		t.Errorf("second batch body=%q want only the 1001st key", got[1].body)
	}
}

// TestS3StoreDeleteObjectsIgnoresBlankKeys keeps callers from having to filter:
// the finalizer builds its key list straight from catalog rows.
func TestS3StoreDeleteObjectsIgnoresBlankKeys(t *testing.T) {
	store, requests := newRecordingStore(t, "viewer/")

	if err := store.DeleteObjects(context.Background(), []string{"", "   "}); err != nil {
		t.Fatalf("DeleteObjects with only blank keys: %v", err)
	}
	if got := *requests; len(got) != 0 {
		t.Fatalf("recorded %d requests, want 0", len(got))
	}
}

// TestS3StoreDeleteObjectsSurfacesLogicalKeysOnError checks that per-key
// failures come back in the caller's namespace, ready to be logged as-is.
func TestS3StoreDeleteObjectsSurfacesLogicalKeysOnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Error>
    <Key>viewer/uploads/album-1.zip</Key>
    <Code>AccessDenied</Code>
    <Message>denied</Message>
  </Error>
</DeleteResult>`)
	}))
	t.Cleanup(server.Close)

	store, err := NewS3Store(context.Background(), cfgpkg.Config{
		S3Endpoint:     server.URL,
		S3Bucket:       "test-bucket",
		S3AccessKey:    "access",
		S3SecretKey:    "secret",
		S3Prefix:       "viewer/",
		S3UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}

	err = store.DeleteObjects(context.Background(), []string{"uploads/album-1.zip"})
	if err == nil {
		t.Fatalf("DeleteObjects returned nil for a failed key")
	}
	if !strings.Contains(err.Error(), "uploads/album-1.zip") || strings.Contains(err.Error(), "viewer/uploads") {
		t.Errorf("error=%v want the logical key without the store prefix", err)
	}
}

// TestIsS3NotFoundTreatsMissingBucketAsAnError pins the distinction the
// backup restore relies on: a missing object is a normal empty answer, while
// a missing bucket is a misconfiguration that has to surface as an error.
func TestIsS3NotFoundTreatsMissingBucketAsAnError(t *testing.T) {
	if !isS3NotFound(&types.NoSuchKey{}) {
		t.Errorf("NoSuchKey should count as not-found")
	}
	if !isS3NotFound(&types.NotFound{}) {
		t.Errorf("NotFound should count as not-found")
	}
	if isS3NotFound(&types.NoSuchBucket{}) {
		t.Errorf("NoSuchBucket is a misconfiguration and must not count as not-found")
	}
}
