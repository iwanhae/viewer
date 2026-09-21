package web

import (
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
