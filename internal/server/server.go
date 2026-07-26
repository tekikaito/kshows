// Package server exposes the JSON API (§8 of the PRD) and the static UI
// bundle over one HTTP listener.
package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/tekikaito/kshows/internal/collector"
	"github.com/tekikaito/kshows/internal/model"
)

type Server struct {
	source collector.Source
	static fs.FS
}

func New(source collector.Source, static fs.FS) *Server {
	return &Server{source: source, static: static}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/v1/stream", s.handleStream)
	mux.HandleFunc("GET /api/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /", http.FileServerFS(s.static))
	return mux
}

func (s *Server) handleSnapshot(w http.ResponseWriter, _ *http.Request) {
	snap := s.source.Latest()
	if snap == nil {
		http.Error(w, `{"error":"no snapshot yet"}`, http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, snap)
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
	fmt.Fprintln(w, "ok")
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

	// Send the current state immediately so a fresh tab paints without
	// waiting a poll interval.
	if snap := s.source.Latest(); snap != nil {
		if err := writeEvent(w, snap); err != nil {
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
			if err := writeEvent(w, snap); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, snap *model.Snapshot) error {
	data, err := json.Marshal(snap)
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
