package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static/*
var staticFiles embed.FS

func Handler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)
		// Vite emits the admin entry beside index.html. It must only be
		// reachable through AdminHandler, which is mounted behind Basic auth.
		if clean == "/admin.html" {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(clean, "/assets/") {
			// Vite content-hashes these names, so a cached copy is good
			// forever. The embedded FS has no modtimes, so without this
			// header the browser would refetch the bundle on every reload.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			fileServer.ServeHTTP(w, r)
			return
		}

		// index.html - served at /, at unknown paths as the SPA fallback, and
		// for any other embedded file - points at whichever bundle this
		// binary ships, so the browser must re-check it on every navigation.
		w.Header().Set("Cache-Control", "no-cache")
		if clean == "/" {
			http.ServeFileFS(w, r, sub, "index.html")
			return
		}
		if _, err := fs.Stat(sub, clean[1:]); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		http.ServeFileFS(w, r, sub, "index.html")
	})
}

// AdminHandler serves the independent admin React entry and its Vite assets.
// The caller must place the entire handler behind admin authentication: this
// deliberately serves the client shell only from the /admin router, not from
// the public viewer SPA fallback.
func AdminHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)
		if strings.HasPrefix(clean, "/admin/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			assetRequest := r.Clone(r.Context())
			assetURL := *r.URL
			assetURL.Path = strings.TrimPrefix(clean, "/admin")
			assetRequest.URL = &assetURL
			fileServer.ServeHTTP(w, assetRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		page, err := fs.ReadFile(sub, "admin.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// Keep the viewer's normal /assets URLs while placing the admin bundle
		// under its authenticated /admin namespace.
		page = bytes.ReplaceAll(page, []byte("/assets/"), []byte("/admin/assets/"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
}
