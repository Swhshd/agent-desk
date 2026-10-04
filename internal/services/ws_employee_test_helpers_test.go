package services

import (
	"encoding/json"
	"testing"
	"time"

	"agent-desk/internal/pkg/dto"
)

type capturedRealtimeEvent struct {
	EventID string         `json:"eventId"`
	Type    string         `json:"type"`
	Topic   string         `json:"topic"`
	Data    map[string]any `json:"data"`
}

// Registration deliberately bypasses admission so receipt tests can exercise
// stale or otherwise unauthorized registry entries.
func captureEmployeeRealtimeSession(t *testing.T, svc *wsService, id string, principal *dto.AuthPrincipal, topics ...string) *ClientSession {
	t.Helper()
	session := &ClientSession{ID: id, Role: realtimeRoleAdmin, Principal: principal, Topics: make(map[string]struct{}), Send: make(chan []byte, realtimeSendBufferSize)}
	svc.manager.Register(session, topics)
	t.Cleanup(func() { svc.manager.Unregister(session) })
	return session
}

func requireCapturedRealtimeEvent(t *testing.T, session *ClientSession, eventType string) capturedRealtimeEvent {
	t.Helper()
	select {
	case body, ok := <-session.Send:
		if !ok {
			t.Fatal("capture channel closed")
		}
		var event capturedRealtimeEvent
		if err := json.Unmarshal(body, &event); err != nil {
			t.Fatalf("decode captured event: %v", err)
		}
		if event.Type != eventType {
			t.Fatalf("event type = %q, want %q", event.Type, eventType)
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %q", eventType)
		return capturedRealtimeEvent{}
	}
}

func requireNoCapturedRealtimeEvent(t *testing.T, session *ClientSession) {
	t.Helper()
	select {
	case body := <-session.Send:
		var event capturedRealtimeEvent
		if err := json.Unmarshal(body, &event); err != nil {
			t.Fatalf("unexpected unreadable event: %v", err)
		}
		t.Fatalf("unexpected event type %q", event.Type)
	default:
	}
}
