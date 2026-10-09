package webui

import (
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticAssetHandlerCompressionCachingAndFallbacks(t *testing.T) {
	root, err := fs.Sub(assets, "static")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newStaticAssetHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	scriptPath := frontendScriptPath(t)
	request := httptest.NewRequest(http.MethodGet, scriptPath, nil)
	request.Header.Set("Accept-Encoding", "gzip, deflate")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("compressed response status=%d headers=%v", response.Code, response.Header())
	}
	original, err := fs.ReadFile(root, scriptPath[1:])
	if err != nil {
		t.Fatal(err)
	}
	uncompressedRequest := httptest.NewRequest(http.MethodGet, scriptPath, nil)
	uncompressedRequest.Header.Set("Accept-Encoding", "br, gzip;q=0.0")
	uncompressedResponse := httptest.NewRecorder()
	handler.ServeHTTP(uncompressedResponse, uncompressedRequest)
	if uncompressedResponse.Header().Get("Content-Encoding") != "" || uncompressedResponse.Body.Len() != len(original) {
		t.Fatalf("disabled gzip response headers=%v body=%d", uncompressedResponse.Header(), uncompressedResponse.Body.Len())
	}
	if response.Header().Get("Cache-Control") == "" || response.Header().Get("ETag") == "" || response.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("static cache headers are incomplete: %v", response.Header())
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if string(decoded) != string(original) {
		t.Fatalf("compressed asset mismatch: err=%v decoded=%d original=%d", err, len(decoded), len(original))
	}

	cachedRequest := httptest.NewRequest(http.MethodGet, scriptPath, nil)
	cachedRequest.Header.Set("If-None-Match", response.Header().Get("ETag"))
	cachedResponse := httptest.NewRecorder()
	handler.ServeHTTP(cachedResponse, cachedRequest)
	if cachedResponse.Code != http.StatusNotModified || cachedResponse.Body.Len() != 0 {
		t.Fatalf("conditional response status=%d body=%d", cachedResponse.Code, cachedResponse.Body.Len())
	}

	indexResponse := httptest.NewRecorder()
	handler.ServeHTTP(indexResponse, httptest.NewRequest(http.MethodGet, "/", nil))
	if indexResponse.Code != http.StatusOK || indexResponse.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index response status=%d cache=%q", indexResponse.Code, indexResponse.Header().Get("Cache-Control"))
	}

	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if missingResponse.Code != http.StatusNotFound {
		t.Fatalf("missing asset status=%d", missingResponse.Code)
	}
}
