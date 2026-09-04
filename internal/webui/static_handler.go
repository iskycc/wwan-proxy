package webui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

type staticAsset struct {
	contentType string
	body        []byte
	gzipBody    []byte
	etag        string
}

type staticAssetHandler struct {
	assets map[string]staticAsset
}

func newStaticAssetHandler(root fs.FS) (http.Handler, error) {
	handler := &staticAssetHandler{assets: make(map[string]staticAsset)}
	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		contentType := mime.TypeByExtension(path.Ext(name))
		if contentType == "" {
			contentType = http.DetectContentType(body)
		}
		hash := sha256.Sum256(body)
		asset := staticAsset{contentType: contentType, body: body, etag: fmt.Sprintf(`W/"%x"`, hash[:12])}
		if isCompressibleContentType(contentType) {
			var compressed bytes.Buffer
			writer, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
			if err != nil {
				return err
			}
			if _, err := writer.Write(body); err != nil {
				return err
			}
			if err := writer.Close(); err != nil {
				return err
			}
			if compressed.Len() < len(body) {
				asset.gzipBody = compressed.Bytes()
			}
		}
		handler.assets[name] = asset
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("prepare embedded static assets: %w", err)
	}
	return handler, nil
}

func (h *staticAssetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if name != path.Clean(name) || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	asset, ok := h.assets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("ETag", asset.etag)
	w.Header().Set("Vary", "Accept-Encoding")
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		// Asset names are stable across releases, so always revalidate instead
		// of risking an old script being paired with a new index after upgrade.
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	}
	if r.Header.Get("If-None-Match") == asset.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := asset.body
	if len(asset.gzipBody) > 0 && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body = asset.gzipBody
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func isCompressibleContentType(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "javascript") || strings.Contains(contentType, "json") || strings.Contains(contentType, "svg+xml")
}

func acceptsGzip(header string) bool {
	for _, value := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(value), ";")
		if !strings.EqualFold(parts[0], "gzip") && parts[0] != "*" {
			continue
		}
		for _, parameter := range parts[1:] {
			key, rawQuality, found := strings.Cut(strings.TrimSpace(parameter), "=")
			quality, err := strconv.ParseFloat(rawQuality, 64)
			if found && strings.EqualFold(strings.TrimSpace(key), "q") && err == nil && quality <= 0 {
				return false
			}
		}
		return true
	}
	return false
}
