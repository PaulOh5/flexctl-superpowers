package spa

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// Handler serves a Vite-style SPA: hashed assets under /assets/* with long
// immutable cache, everything else falls back to index.html so the React
// router can handle client-side routes.
//
// API routes (/v1/*) and health probes must be registered BEFORE this handler
// in the chi mux (typically via r.NotFound(spaHandler.ServeHTTP)).
type Handler struct {
	fs       fs.FS
	indexBuf []byte
}

// New panics if index.html is not present in assets — that's a build invariant
// the embed step (internal/webui) must satisfy.
func New(assets fs.FS) *Handler {
	idx, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		panic("spa: index.html not found in embedded assets — did `make web-build` run?")
	}
	return &Handler{fs: assets, indexBuf: idx}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		h.serveIndex(w)
		return
	}
	f, err := h.fs.Open(p)
	if err != nil {
		h.serveIndex(w)
		return
	}
	defer f.Close()

	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// Some fs.FS implementations (notably testing.fstest.MapFS files via
		// io/fs.ReadFile) don't satisfy ReadSeeker; fall back to a bytes.Reader.
		data, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "read asset", http.StatusInternalServerError)
			return
		}
		rs = bytes.NewReader(data)
	}
	http.ServeContent(w, r, p, time.Time{}, rs)
}

func (h *Handler) serveIndex(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(h.indexBuf)
}
