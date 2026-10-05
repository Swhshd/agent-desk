package services

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-desk/internal/models"
	"agent-desk/internal/pkg/dto"
	"agent-desk/internal/pkg/enums"
	"github.com/glebarez/sqlite"
	"github.com/mlogclub/simple/sqls"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

type capturedRealtimeEvent struct {
	EventID string         `json:"eventId"`
	Type    string         `json:"type"`
	Topic   string         `json:"topic"`
	At      string         `json:"at"`
	Data    map[string]any `json:"data"`
}

var employeePublicationFallback struct {
	once sync.Once
	db   *gorm.DB
	err  error
}

// The asynchronous assignment handlers may already hold one of these pointers
// when the case restores the global DB. Retain only synthetic in-memory handles
// until process exit; each case still has its own uniquely named database.
var employeePublicationRetainedDBs []*gorm.DB
var employeePublicationDBSequence atomic.Uint64

// Assignment callbacks can start after a per-case fixture is restored. Keep a
// live empty database for their initial lookup until the test process exits.
func employeePublicationFallbackDB(t *testing.T) *gorm.DB {
	t.Helper()
	employeePublicationFallback.once.Do(func() {
		employeePublicationFallback.db, employeePublicationFallback.err = gorm.Open(sqlite.Open("file:employee_realtime_callback_fallback?mode=memory&cache=shared"), &gorm.Config{
			NamingStrategy: schema.NamingStrategy{TablePrefix: "t_", SingularTable: true},
		})
		if employeePublicationFallback.err == nil {
			employeePublicationFallback.err = employeePublicationFallback.db.AutoMigrate(humanDispatchRealtimeTestModels()...)
		}
	})
	if employeePublicationFallback.err != nil {
		t.Fatal(employeePublicationFallback.err)
	}
	return employeePublicationFallback.db
}

func setupEmployeePublicationTest(t *testing.T) (*gorm.DB, *wsService) {
	t.Helper()
	sqls.SetDB(employeePublicationFallbackDB(t))
	previousDB, previousWs, previousHook := sqls.DB(), WsService, TriggerAIReplyAsyncHook
	db := setupPersistentHumanDispatchRealtimeTestDB(t)
	WsService = newWsService()
	TriggerAIReplyAsyncHook = nil
	t.Cleanup(func() { sqls.SetDB(previousDB); WsService = previousWs; TriggerAIReplyAsyncHook = previousHook })
	return db, WsService
}

func createEmployeePublicationAgent(t *testing.T, db *gorm.DB, userID, teamID int64) {
	t.Helper()
	if err := db.Create(&models.User{ID: userID, Username: fmt.Sprintf("synthetic-agent-%d", userID), Nickname: "synthetic-agent", Status: enums.StatusOk}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.AgentProfile{UserID: userID, TeamID: teamID, AgentCode: fmt.Sprintf("synthetic-%d", userID), DisplayName: "synthetic-agent", ServiceStatus: enums.ServiceStatusIdle, MaxConcurrentCount: 3, AutoAssignEnabled: true, Status: enums.StatusOk}).Error; err != nil {
		t.Fatal(err)
	}
}

// Exact allowlists catch leaks even when private fields serialize zero values.
func assertEmployeeQueueData(t *testing.T, event capturedRealtimeEvent, allowedKeys []string) {
	t.Helper()
	body, _ := json.Marshal(event.Data)
	t.Logf("serialized queue data: %s", body)
	for key := range event.Data {
		if !slices.Contains(allowedKeys, key) {
			t.Errorf("queue serialized forbidden key %q", key)
		}
	}
	var inspect func(any)
	inspect = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			for key, child := range item {
				if slices.Contains([]string{"message", "content", "payload", "lastMessageSummary", "senderName", "senderAvatar", "customerName", "customerContact", "customerProfile", "history", "attachments", "attachment", "customerLastReadMessageId", "customerLastReadAt", "agentLastReadMessageId", "agentLastReadAt"}, key) {
					t.Errorf("queue serialized prohibited field %q", key)
				}
				inspect(child)
			}
		case []any:
			for _, child := range item {
				inspect(child)
			}
		}
	}
	inspect(event.Data)
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
