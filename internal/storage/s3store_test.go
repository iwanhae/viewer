package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
	"time"

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
	method string
	host   string
	path   string
	query  string
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
		requests = append(requests, recordedRequest{
			method: r.Method,
			host:   r.Host,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
		})
		switch {
		case r.URL.Query().Get("list-type") == "2":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, listObjectsXML)
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "11")
			w.Header().Set("ETag", `"etag-1"`)
			w.Header().Set("Content-Type", "image/png")
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

	if err := store.DeleteObject(ctx, "blobs/deadbeef"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	want := []struct{ method, path string }{
		{http.MethodPut, "/test-bucket/viewer/blobs/deadbeef"},
		{http.MethodGet, "/test-bucket/viewer/blobs/deadbeef"},
		{http.MethodHead, "/test-bucket/viewer/blobs/deadbeef"},
		{http.MethodDelete, "/test-bucket/viewer/blobs/deadbeef"},
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

	// A listed key must round-trip through the other methods unchanged.
	if err := store.DeleteObject(context.Background(), objects[0].Key); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if path := (*requests)[1].path; path != "/test-bucket/viewer/uploads/first.zip" {
		t.Errorf("delete path=%q want /test-bucket/viewer/uploads/first.zip", path)
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
// PUTs to, which never goes through the store's own client.
func TestS3StorePresignsUnderKeyPrefix(t *testing.T) {
	store, _ := newRecordingStore(t, "team-a/")

	url, headers, err := store.PresignPut(context.Background(), "uploads/album-1.zip", time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if !strings.Contains(url, "/test-bucket/team-a/uploads/album-1.zip") {
		t.Errorf("presigned url=%q want the bucket and key prefix in the path", url)
	}
	if len(headers) != 0 {
		t.Errorf("headers=%v want none", headers)
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

			url, _, err := store.PresignPut(context.Background(), "uploads/album-1.zip", time.Minute)
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
