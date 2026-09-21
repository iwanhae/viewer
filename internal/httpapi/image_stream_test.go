package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
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

// TestGetImageServesScaledVariant exercises the w= ladder: wide originals come
// back as resampled JPEGs with a width-suffixed validator, small originals
// pass through untouched, and unsupported widths are rejected.
func TestGetImageServesScaledVariant(t *testing.T) {
	harness := newFlowHarness(t)

	imageA := testPNG(t, 400, 200, 12)
	imageB := testPNG(t, 13, 7, 40)
	zipData := testZip(t,
		map[string][]byte{"001.png": imageA, "002.png": imageB},
		[]string{"002.png", "001.png"},
	)
	albumID := harness.uploadZip(t, "scaled.zip", zipData)
	harness.finalizeAndWait(t, albumID)

	photo, err := harness.catalog.PhotoAt(context.Background(), albumID, 0)
	if err != nil {
		t.Fatalf("photo at 0: %v", err)
	}

	// A wide original is served resampled to the requested width.
	scaledRec := harness.do(t, http.MethodGet, fmt.Sprintf("/api/image/%s/0?w=320", albumID), nil)
	if scaledRec.Code != http.StatusOK {
		t.Fatalf("scaled status=%d body=%s", scaledRec.Code, scaledRec.Body.String())
	}
	if got := scaledRec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("scaled content-type=%q want=image/jpeg", got)
	}
	if got, want := scaledRec.Header().Get("ETag"), `"`+photo.Hash+`:w320"`; got != want {
		t.Fatalf("scaled etag=%q want=%q", got, want)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(scaledRec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode scaled body: %v", err)
	}
	if config.Width != 320 {
		t.Fatalf("scaled width=%d want=320", config.Width)
	}

	// A small original is passed through with its own bytes and validator.
	smallRec := harness.do(t, http.MethodGet, fmt.Sprintf("/api/image/%s/1?w=640", albumID), nil)
	if smallRec.Code != http.StatusOK {
		t.Fatalf("small status=%d body=%s", smallRec.Code, smallRec.Body.String())
	}
	if !bytes.Equal(smallRec.Body.Bytes(), imageB) {
		t.Fatalf("small original should pass through unchanged")
	}
	if got := smallRec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("small content-type=%q want=image/png", got)
	}

	// Off-ladder widths are rejected instead of silently resampled.
	offRec := harness.do(t, http.MethodGet, fmt.Sprintf("/api/image/%s/0?w=500", albumID), nil)
	if offRec.Code != http.StatusBadRequest {
		t.Fatalf("off-ladder status=%d body=%s", offRec.Code, offRec.Body.String())
	}

	// Without w the endpoint still serves the untouched original.
	plainRec := harness.do(t, http.MethodGet, fmt.Sprintf("/api/image/%s/0", albumID), nil)
	if plainRec.Code != http.StatusOK || !bytes.Equal(plainRec.Body.Bytes(), imageA) {
		t.Fatalf("plain request must serve the original: %d", plainRec.Code)
	}
}
