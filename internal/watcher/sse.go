package watcher

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// keepaliveInterval bounds how long a stream can sit silent. Proxies and load
// balancers drop idle connections, so a comment frame goes out periodically.
const keepaliveInterval = 25 * time.Second

// subscriberBuffer is how many state changes a slow client may fall behind
// before it is dropped. State messages are snapshots, not deltas, so a dropped
// subscriber loses nothing it cannot recover by reconnecting or polling.
const subscriberBuffer = 8

// Hub fans session state changes out to connected SSE clients.
//
// This is deliberately in-process, which is correct only while the service runs
// as a single instance - every mutation happens in this process, so every
// mutation can be broadcast from it. Running more than one replica requires
// moving the fan-out to a MongoDB change stream on watcher_sessions; see the
// migration plan. fly.toml pins the machine count to 1 for this reason.
type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[chan PublicSession]struct{}
}

// NewHub builds an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[chan PublicSession]struct{})}
}

// Subscribe registers a listener for one session's state changes.
func (h *Hub) Subscribe(sessionID string) chan PublicSession {
	ch := make(chan PublicSession, subscriberBuffer)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = make(map[chan PublicSession]struct{})
	}
	h.subs[sessionID][ch] = struct{}{}
	return ch
}

// Unsubscribe removes a listener and releases the session entry when it was the
// last one.
func (h *Hub) Unsubscribe(sessionID string, ch chan PublicSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	listeners, ok := h.subs[sessionID]
	if !ok {
		return
	}
	if _, ok := listeners[ch]; !ok {
		return
	}
	delete(listeners, ch)
	close(ch)
	if len(listeners) == 0 {
		delete(h.subs, sessionID)
	}
}

// Broadcast delivers state to every listener on a session. Listeners that are
// not keeping up are skipped rather than blocking the writer.
func (h *Hub) Broadcast(sessionID string, state PublicSession) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[sessionID] {
		select {
		case ch <- state:
		default:
			slog.Warn("dropping session event for slow subscriber",
				"session_id", sessionID)
		}
	}
}

// subscriberCount reports how many listeners a session has. Used by tests.
func (h *Hub) subscriberCount(sessionID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[sessionID])
}

// events streams session state as Server-Sent Events.
//
// This endpoint is additive: the polling route it replaces
// (GET /api/watcher/sessions/{id}) is unchanged, so a client that cannot use
// SSE - or a deployment that needs to be rolled back - keeps working.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	ctx, ok := participantFrom(r.Context())
	if !ok {
		http.Error(w, "missing participant context", http.StatusUnauthorized)
		return
	}

	// ResponseController rather than a direct http.Flusher assertion: the
	// logging middleware wraps the ResponseWriter, and the controller follows
	// the Unwrap chain to reach the real one.
	rc := http.NewResponseController(w)

	// Clear the server's WriteTimeout for this connection. A stream outlives it
	// by design, and without this the connection is severed mid-session.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		slog.Warn("could not clear write deadline for SSE stream", "error", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disables response buffering in reverse proxies that honour it.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	events := h.hub.Subscribe(ctx.SessionID)
	defer h.hub.Unsubscribe(ctx.SessionID, events)

	// Send the current state immediately so a subscriber never has to wait for
	// the next mutation to render.
	writeEvent(w, rc, ToPublic(ctx.Session))

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	// The stream must not outlive the session it belongs to.
	expiry := time.NewTimer(time.Until(ctx.Session.ExpiresAt))
	defer expiry.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-expiry.C:
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			_ = rc.Flush()
		case state, open := <-events:
			if !open {
				return
			}
			writeEvent(w, rc, state)
		}
	}
}

// writeEvent emits one SSE state frame.
func writeEvent(w http.ResponseWriter, rc *http.ResponseController, state PublicSession) {
	payload, err := json.Marshal(state)
	if err != nil {
		slog.Error("failed to marshal session event", "error", err)
		return
	}
	if _, err := w.Write([]byte("event: state\ndata: " + string(payload) + "\n\n")); err != nil {
		return
	}
	_ = rc.Flush()
}
