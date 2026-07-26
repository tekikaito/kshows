// Package server exposes the JSON API (§8 of the PRD) and the static UI
// bundle over one HTTP listener.
package server

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tekikaito/kshows/internal/collector"
	"github.com/tekikaito/kshows/internal/metrics"
	"github.com/tekikaito/kshows/internal/model"
)

type Server struct {
	source collector.Source
	static fs.FS
	cache  marshalCache
}

// marshalCache holds the JSON encoding of the most recent snapshot. The
// collector hands every subscriber (and Latest caller) the same immutable
// pointer per tick, so pointer identity is a safe cache key — one marshal
// per snapshot instead of one per connected client.
type marshalCache struct {
	mu   sync.Mutex
	snap *model.Snapshot
	data []byte
}

func (c *marshalCache) bytes(snap *model.Snapshot) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if snap == c.snap && c.data != nil {
		return c.data, nil
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	c.snap, c.data = snap, data
	return data, nil
}

func New(source collector.Source, static fs.FS) *Server {
	metrics.WatchSnapshots(source.Latest)
	return &Server{source: source, static: static}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/v1/stream", s.handleStream)
	mux.HandleFunc("GET /api/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// A probe that hung up mid-response leaves us nothing to act on.
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /metrics", metrics.Handler())
	mux.Handle("GET /", http.FileServerFS(s.static))
	return mux
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.source.Latest()
	if snap == nil {
		http.Error(w, `{"error":"no snapshot yet"}`, http.StatusServiceUnavailable)
		return
	}
	data, err := s.cache.bytes(snap)
	if err != nil {
		http.Error(w, `{"error":"encoding snapshot"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// gzip only this endpoint: the SSE stream must flush partial writes,
	// which a gzip writer would buffer and break.
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, werr := gz.Write(data)
		if cerr := gz.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			log.Printf("writing snapshot: %v", werr)
		}
		return
	}
	if _, err := w.Write(data); err != nil {
		log.Printf("writing snapshot: %v", err)
	}
}

func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	snap := s.source.Latest()
	caps := model.Capabilities{}
	if snap != nil {
		caps = snap.Capabilities
	}
	writeJSON(w, caps)
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.source.Latest() == nil {
		http.Error(w, "no snapshot yet", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

// handleStream is the SSE endpoint. Each event is a full snapshot; the client
// keys rendering by pod UID, so identity is stable across events and blocks
// animate instead of flickering.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, cancel := s.source.Subscribe()
	defer cancel()
	metrics.StreamConnected()
	defer metrics.StreamDisconnected()

	// Send the current state immediately so a fresh tab paints without
	// waiting a poll interval.
	if snap := s.source.Latest(); snap != nil {
		if err := s.writeEvent(w, snap); err != nil {
			return
		}
		flusher.Flush()
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// Comment line keeps proxies from timing the connection out.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case snap := <-ch:
			if err := s.writeEvent(w, snap); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) writeEvent(w http.ResponseWriter, snap *model.Snapshot) error {
	data, err := s.cache.bytes(snap)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data)
	return err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writing response: %v", err)
	}
}
