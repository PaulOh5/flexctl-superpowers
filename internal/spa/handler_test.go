package spa_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/spa"
)

func newTestFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><html>SPA</html>")},
		"assets/main.abc.js":  {Data: []byte("console.log('hi')")},
		"favicon.ico":         {Data: []byte("\x00\x00\x01")},
	}
}

func TestSPA_ServesExistingFile(t *testing.T) {
	h := spa.New(newTestFS())
	req := httptest.NewRequest(http.MethodGet, "/assets/main.abc.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "console.log('hi')", rec.Body.String())
	require.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
}

func TestSPA_FallsBackToIndexForUnknownRoute(t *testing.T) {
	h := spa.New(newTestFS())
	req := httptest.NewRequest(http.MethodGet, "/envs/new", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "<!doctype html>")
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
}

func TestSPA_RootServesIndex(t *testing.T) {
	h := spa.New(newTestFS())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "<!doctype html>")
}

func TestSPA_NonGetReturnsNotFound(t *testing.T) {
	h := spa.New(newTestFS())
	req := httptest.NewRequest(http.MethodPost, "/anywhere", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSPA_PanicsIfNoIndex(t *testing.T) {
	defer func() {
		r := recover()
		require.NotNil(t, r, "expected panic when index.html missing")
	}()
	_ = spa.New(fstest.MapFS{})
}
