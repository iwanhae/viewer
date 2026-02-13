package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
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
