// Package api serves the topology REST endpoints and the WebSocket diff stream
// consumed by the dashboard. It is a thin read-only view over the collector.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/Ramtinboreili/Pulsebox/internal/collector"
)

// Server bundles the collector with HTTP handlers.
type Server struct {
	col      *collector.Collector
	upgrader websocket.Upgrader
}

// New builds an API server.
func New(col *collector.Collector) *Server {
	return &Server{
		col: col,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 16 * 1024,
			// Internal-only service with no auth (v1); allow any origin. Put
			// Keycloak/oauth2-proxy in front for anything sensitive.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// RegisterRoutes mounts the API onto a mux (Go 1.22+ pattern matching).
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/topology", s.handleTopology)
	mux.HandleFunc("GET /api/topology/{namespace}", s.handleTopologyNamespace)
	mux.HandleFunc("GET /api/resource/{kind}/{namespace}/{name}", s.handleResource)
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("GET /api/health", s.handleHealth)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encode error: %v", err)
	}
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.col.Snapshot())
}

func (s *Server) handleTopologyNamespace(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	writeJSON(w, http.StatusOK, s.col.NamespaceSnapshot(ns))
}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	// A literal "-" in the namespace slot denotes a cluster-scoped object.
	if ns == "-" {
		ns = ""
	}
	obj, err := s.col.GetResource(kind, ns, name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	// Strip managedFields to keep the payload lean.
	if acc, err := meta.Accessor(obj); err == nil {
		acc.SetManagedFields(nil)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"kind":          kind,
		"namespace":     ns,
		"name":          name,
		"resource":      obj,
		"warningEvents": s.col.WarningEvents(canonicalKind(kind), ns, name, 10),
	})
}

// handleHealth is Pulsebox's OWN liveness/readiness endpoint — distinct from
// cluster health. It reports 200 when serving; readiness (?ready=1) additionally
// requires the informer caches to be synced.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	synced := s.col.Synced()
	code := http.StatusOK
	if r.URL.Query().Get("ready") != "" && !synced {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]interface{}{
		"status":      "ok",
		"synced":      synced,
		"subscribers": s.col.StreamSubscribers(),
		"time":        time.Now().UTC(),
	})
}

const (
	writeWait  = 10 * time.Second
	pingPeriod = 30 * time.Second
)

// handleStream upgrades to a WebSocket, sends the current snapshot, then streams
// incremental diffs as the informer caches change.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader already wrote the error
	}
	defer conn.Close()

	snapshot, diffs, unsubscribe := s.col.Subscribe()
	defer unsubscribe()

	// Reader pump: we don't expect client messages, but reading is required to
	// process control frames (pong/close) and detect disconnects.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	writeMsg := func(v interface{}) error {
		conn.SetWriteDeadline(time.Now().Add(writeWait))
		return conn.WriteJSON(v)
	}

	if err := writeMsg(snapshot); err != nil {
		return
	}

	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-closed:
			return
		case d, ok := <-diffs:
			if !ok {
				// Dropped by the broker (slow consumer); let the client reconnect.
				return
			}
			if err := writeMsg(d); err != nil {
				return
			}
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// canonicalKind maps user-facing kind aliases to the Kubernetes Kind used on
// event InvolvedObject references.
func canonicalKind(kind string) string {
	switch strings.ToLower(kind) {
	case "pvc", "persistentvolumeclaim":
		return "PersistentVolumeClaim"
	case "pv", "persistentvolume":
		return "PersistentVolume"
	case "pod":
		return "Pod"
	case "node":
		return "Node"
	case "namespace":
		return "Namespace"
	case "deployment":
		return "Deployment"
	case "statefulset":
		return "StatefulSet"
	case "daemonset":
		return "DaemonSet"
	case "service":
		return "Service"
	default:
		return kind
	}
}
