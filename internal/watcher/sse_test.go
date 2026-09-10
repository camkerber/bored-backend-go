package watcher

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func TestHubDeliversToSubscriber(t *testing.T) {
	hub := NewHub()
	events := hub.Subscribe("session-1")
	defer hub.Unsubscribe("session-1", events)

	hub.Broadcast("session-1", PublicSession{SessionID: "session-1", Status: StatusMatching})

	select {
	case got := <-events:
		assert.Equal(t, StatusMatching, got.Status)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the broadcast")
	}
}

func TestHubDeliversToEverySubscriberOfASession(t *testing.T) {
	hub := NewHub()
	first := hub.Subscribe("session-1")
	second := hub.Subscribe("session-1")
	defer hub.Unsubscribe("session-1", first)
	defer hub.Unsubscribe("session-1", second)

	hub.Broadcast("session-1", PublicSession{SessionID: "session-1", Rounds: 3})

	for _, ch := range []chan PublicSession{first, second} {
		select {
		case got := <-ch:
			assert.Equal(t, 3, got.Rounds)
		case <-time.After(time.Second):
			t.Fatal("a subscriber missed the broadcast")
		}
	}
}

func TestHubIsolatesSessions(t *testing.T) {
	hub := NewHub()
	mine := hub.Subscribe("session-1")
	theirs := hub.Subscribe("session-2")
	defer hub.Unsubscribe("session-1", mine)
	defer hub.Unsubscribe("session-2", theirs)

	hub.Broadcast("session-1", PublicSession{SessionID: "session-1"})

	require.Len(t, mine, 1, "the targeted session must receive the event")
	assert.Empty(t, theirs, "an unrelated session must not receive it")
}

func TestHubUnsubscribeClosesAndReleases(t *testing.T) {
	hub := NewHub()
	events := hub.Subscribe("session-1")
	require.Equal(t, 1, hub.subscriberCount("session-1"))

	hub.Unsubscribe("session-1", events)

	assert.Equal(t, 0, hub.subscriberCount("session-1"))
	_, open := <-events
	assert.False(t, open, "the channel must be closed so the stream loop exits")
}

// TestHubUnsubscribeIsIdempotent guards against a double close panic: the SSE
// handler unsubscribes via defer, and an early return path could do it twice.
func TestHubUnsubscribeIsIdempotent(t *testing.T) {
	hub := NewHub()
	events := hub.Subscribe("session-1")

	hub.Unsubscribe("session-1", events)

	assert.NotPanics(t, func() {
		hub.Unsubscribe("session-1", events)
		hub.Unsubscribe("unknown-session", events)
	})
}

// TestBroadcastDoesNotBlockOnASlowSubscriber is the property that keeps one
// stalled browser tab from freezing every mutation in the process.
func TestBroadcastDoesNotBlockOnASlowSubscriber(t *testing.T) {
	hub := NewHub()
	events := hub.Subscribe("session-1")
	defer hub.Unsubscribe("session-1", events)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than the subscriber buffer, and nothing is draining it.
		for i := range subscriberBuffer * 10 {
			hub.Broadcast("session-1", PublicSession{Rounds: i})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast blocked on a subscriber that was not draining")
	}
}

func TestBroadcastToSessionWithNoSubscribers(t *testing.T) {
	hub := NewHub()

	assert.NotPanics(t, func() {
		hub.Broadcast("nobody-is-listening", PublicSession{})
	})
}

// TestHubConcurrentAccess is meaningful under -race, which CI runs.
func TestHubConcurrentAccess(t *testing.T) {
	hub := NewHub()
	var wg sync.WaitGroup

	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessionID := "session-" + string(rune('a'+i%5))
			events := hub.Subscribe(sessionID)
			hub.Broadcast(sessionID, PublicSession{SessionID: sessionID})
			<-events
			hub.Unsubscribe(sessionID, events)
		}()
	}

	wg.Wait()
	for i := range 5 {
		assert.Equal(t, 0, hub.subscriberCount("session-"+string(rune('a'+i))))
	}
}
