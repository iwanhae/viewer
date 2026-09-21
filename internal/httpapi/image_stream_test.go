package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetImageServesBlobWithHTTPMetadata exercises the streaming image read
// path end to end: the response must carry a length, an ETag derived from the
// blob hash, revalidation and range support.
func TestGetImageServesBlobWithHTTPMetadata(t *testing.T) {
	harness := newFlowHarness(t)

	imageA := testPNG(t, 8, 4, 33)
	zipData := testZip(t, map[string][]byte{"only.png": imageA}, []string{"only.png"})
	albumID := harness.uploadZip(t, "stream.zip", zipData)
	harness.finalizeAndWait(t, albumID)

	photo, err := harness.catalog.PhotoAt(context.Background(), albumID, 0)
	if err != nil {
		t.Fatalf("photo at 0: %v", err)
	}
	path := fmt.Sprintf("/api/image/%s/0", albumID)

	rec := harness.do(t, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), imageA) {
		t.Fatalf("served bytes do not match the stored image")
	}
	if got, want := rec.Header().Get("Content-Length"), fmt.Sprint(len(imageA)); got != want {
		t.Fatalf("content-length=%q want=%q", got, want)
	}
	if got, want := rec.Header().Get("ETag"), `"`+photo.Hash+`"`; got != want {
		t.Fatalf("etag=%q want=%q", got, want)
	}
	if got, want := rec.Header().Get("Cache-Control"), "public, max-age=86400, immutable"; got != want {
		t.Fatalf("cache-control=%q want=%q", got, want)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("accept-ranges=%q want=bytes", got)
	}

	// A matching validator is answered with 304 and no body.
	condReq := httptest.NewRequest(http.MethodGet, path, nil)
	condReq.Header.Set("If-None-Match", `"`+photo.Hash+`"`)
	condRec := httptest.NewRecorder()
	harness.router.ServeHTTP(condRec, condReq)
	if condRec.Code != http.StatusNotModified {
		t.Fatalf("conditional status=%d want=304", condRec.Code)
	}
	if condRec.Body.Len() != 0 {
		t.Fatalf("conditional body=%d bytes want=0", condRec.Body.Len())
	}

	// A range request is answered from the in-memory copy of the blob.
	rangeReq := httptest.NewRequest(http.MethodGet, path, nil)
	rangeReq.Header.Set("Range", "bytes=1-3")
	rangeRec := httptest.NewRecorder()
	harness.router.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent {
		t.Fatalf("range status=%d want=206 body=%s", rangeRec.Code, rangeRec.Body.String())
	}
	if !bytes.Equal(rangeRec.Body.Bytes(), imageA[1:4]) {
		t.Fatalf("range bytes=%v want=%v", rangeRec.Body.Bytes(), imageA[1:4])
	}
}
