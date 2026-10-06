package services

import (
	"bytes"
	"testing"
)

func TestClientSessionEnqueueCharacterization(t *testing.T) {
	var nilSession *ClientSession
	if nilSession.enqueue([]byte("nil")) {
		t.Fatal("enqueue on a nil session = true, want false")
	}

	payload := []byte("payload")
	openSession := &ClientSession{Send: make(chan []byte, 1)}
	if !openSession.enqueue(payload) {
		t.Fatal("enqueue on an open session with room = false, want true")
	}
	if got := <-openSession.Send; !bytes.Equal(got, payload) {
		t.Fatalf("queued payload = %q, want %q", got, payload)
	}

	fullSession := &ClientSession{Send: make(chan []byte, 1)}
	queued := []byte("already queued")
	fullSession.Send <- queued
	if fullSession.enqueue(payload) {
		t.Fatal("enqueue on a full buffer = true, want false")
	}
	if got := <-fullSession.Send; !bytes.Equal(got, queued) {
		t.Fatalf("queued payload after full-buffer enqueue = %q, want %q", got, queued)
	}

	closedSession := &ClientSession{Send: make(chan []byte, 1)}
	closedSession.Closed.Store(true)
	if closedSession.enqueue(payload) {
		t.Fatal("enqueue on a closed session = true, want false")
	}
	if got := len(closedSession.Send); got != 0 {
		t.Fatalf("closed session queued %d payloads, want 0", got)
	}
}
