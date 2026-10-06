package services

import (
	"bytes"
	"errors"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-desk/internal/pkg/dto"
	"agent-desk/internal/pkg/openidentity"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestWsSessionNormalDelivery(t *testing.T) {
	service, peer, session := newWsPumpTestSocket(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.writePump(session)
	}()
	t.Cleanup(func() {
		service.closeSession(session)
		waitWsPumpDone(t, done, "writer")
	})

	payload := []byte("normal delivery")
	if !session.enqueue(payload) {
		t.Fatal("enqueue on open session = false, want true")
	}
	assertWsPeerText(t, peer, payload)
}

func TestWsWriterPumpTerminatesAfterSessionClose(t *testing.T) {
	service, _, session := newWsPumpTestSocket(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.writePump(session)
	}()
	service.closeSession(session)
	waitWsPumpDone(t, done, "writer")
}

func TestWsReaderPumpTerminatesAfterSessionClose(t *testing.T) {
	service, peer, session := newWsPumpTestSocket(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.readPump(session)
	}()
	if err := peer.WriteJSON(map[string]string{"type": "ping"}); err != nil {
		t.Fatalf("send reader pump probe: %v", err)
	}
	select {
	case payload, ok := <-session.Send:
		if !ok || !bytes.Contains(payload, []byte(`"type":"pong"`)) {
			t.Fatalf("reader pump probe response = %q, open=%t; want pong", payload, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader pump did not process probe within two seconds")
	}
	service.closeSession(session)
	waitWsPumpDone(t, done, "reader")
}

func TestWsSessionCloseAcrossHandlerRoles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		role  string
		topic string
		open  func(*testing.T, *wsService) *websocket.Conn
	}{
		{
			name: "dashboard employee", role: realtimeRoleAdmin, topic: "admin:101",
			open: func(t *testing.T, service *wsService) *websocket.Conn {
				return openWsDashboardPumpTestSocket(t, service, 101, service.HandleDashboardWS)
			},
		},
		{
			name: "dashboard notification", role: realtimeRoleNotification, topic: "notification:102",
			open: func(t *testing.T, service *wsService) *websocket.Conn {
				return openWsDashboardPumpTestSocket(t, service, 102, service.HandleDashboardNotificationWS)
			},
		},
		{
			name: "synthetic customer/open", role: realtimeRoleUser, topic: "customer:103",
			open: func(t *testing.T, service *wsService) *websocket.Conn {
				return openCustomerIdentityTestSocket(t, service, 103, &openidentity.ExternalUser{ExternalID: "pump-test-customer"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newWsService()
			peer := tc.open(t, service)
			if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if messageType, _, err := peer.ReadMessage(); err != nil || messageType != websocket.TextMessage {
				t.Fatalf("connected frame: type=%d, error=%v", messageType, err)
			}
			members := service.manager.FindByTopics([]string{tc.topic})
			if len(members) != 1 || members[0].Role != tc.role {
				t.Fatalf("registered %s sessions = %v, want one %s session", tc.topic, members, tc.role)
			}
			session := members[0]
			t.Cleanup(func() { service.closeSession(session) })
			payload := []byte("queued role delivery")
			if !session.enqueue(payload) {
				t.Fatal("enqueue on handler session = false, want true")
			}
			assertWsPeerText(t, peer, payload)

			service.closeSession(session)
			if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := peer.ReadMessage(); err == nil {
				t.Fatal("peer read succeeded after server-side close")
			} else {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					t.Fatalf("peer read timed out after server-side close: %v", err)
				}
			}
			if !session.Closed.Load() || len(service.manager.FindByTopics([]string{tc.topic})) != 0 {
				t.Fatal("closed handler session remains open or registered")
			}
		})
	}
}

func newWsPumpTestSocket(t *testing.T) (*wsService, *websocket.Conn, *ClientSession) {
	t.Helper()
	service := newWsService()
	sessions := make(chan *ClientSession, 1)
	router := gin.New()
	router.GET("/ws", func(ctx *gin.Context) {
		conn, err := service.upgrader.Upgrade(ctx.Writer, ctx.Request, nil)
		if err != nil {
			return
		}
		sessions <- &ClientSession{Conn: conn, Topics: make(map[string]struct{}), Send: make(chan []byte, realtimeSendBufferSize)}
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("pump test handshake: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	select {
	case session := <-sessions:
		t.Cleanup(func() { service.closeSession(session) })
		return service, peer, session
	case <-time.After(2 * time.Second):
		t.Fatal("server-side Gorilla connection was not received")
		return nil, nil, nil
	}
}

func openWsDashboardPumpTestSocket(t *testing.T, service *wsService, userID int64, handler gin.HandlerFunc) *websocket.Conn {
	t.Helper()
	router := gin.New()
	router.GET("/ws", func(ctx *gin.Context) {
		ctx.Set(authPrincipalContextKey, &dto.AuthPrincipal{UserID: userID})
		handler(ctx)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dashboard handshake: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	return peer
}

func assertWsPeerText(t *testing.T, peer *websocket.Conn, want []byte) {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, got, err := peer.ReadMessage()
	if err != nil {
		t.Fatalf("read queued text: %v", err)
	}
	if messageType != websocket.TextMessage || !bytes.Equal(got, want) {
		t.Fatalf("queued frame = (type %d, %q), want text %q", messageType, got, want)
	}
}

func waitWsPumpDone(t *testing.T, done <-chan struct{}, pump string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s pump did not terminate within two seconds", pump)
	}
}

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

func TestClientSessionEnqueueCloseLinearization(t *testing.T) {
	service := newWsService()
	session := &ClientSession{Send: make(chan []byte, 1)}
	observedOpen := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	defer release()

	type enqueueOutcome struct {
		queued     bool
		panicValue any
	}
	outcomes := make(chan enqueueOutcome, 1)
	go func() {
		outcome := enqueueOutcome{}
		defer func() {
			outcome.panicValue = recover()
			outcomes <- outcome
		}()
		outcome.queued = session.enqueueWithBeforeSend([]byte("payload"), func() {
			close(observedOpen)
			<-resume
		})
	}()

	select {
	case <-observedOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue did not reach the open-observation barrier")
	}

	closed := make(chan struct{})
	go func() {
		service.closeSession(session)
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closeSession blocked while enqueue was at the barrier")
	}
	select {
	case payload, ok := <-session.Send:
		if ok {
			t.Fatalf("Send yielded %q before barrier release, want closed channel", payload)
		}
	default:
		t.Fatal("Send is still open after closeSession completed")
	}

	release()
	select {
	case outcome := <-outcomes:
		if outcome.panicValue != nil {
			t.Errorf("enqueue panicked: %v; want no panic", outcome.panicValue)
		}
		if outcome.queued {
			t.Error("enqueue after close = true, want false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue did not complete after barrier release")
	}
	if got := len(session.Send); got != 0 {
		t.Fatalf("queued payloads after close = %d, want 0", got)
	}
}

func TestWsSessionConcurrentEnqueueClose(t *testing.T) {
	for round := 0; round < 32; round++ {
		service, session, topic := newWsSessionConcurrencyFixture("concurrent-enqueue-close")
		workers := make([]func(), 0, 40)
		for producer := 0; producer < 32; producer++ {
			workers = append(workers, func() {
				for attempt := 0; attempt < 128; attempt++ {
					session.enqueue([]byte("payload"))
				}
			})
		}
		for closer := 0; closer < 8; closer++ {
			workers = append(workers, func() { service.closeSession(session) })
		}
		runWsSessionConcurrencyWorkers(t, workers)
		assertWsSessionCleanup(t, service, session, topic)
		assertWsSessionSendClosed(t, session)
	}
}

func TestWsSessionConcurrentClose(t *testing.T) {
	service, session, topic := newWsSessionConcurrencyFixture("concurrent-close")
	var logs wsSessionConcurrencyLogBuffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previousLogger)

	workers := make([]func(), 32)
	for i := range workers {
		workers[i] = func() { service.closeSession(session) }
	}
	runWsSessionConcurrencyWorkers(t, workers)
	service.closeSession(session)
	assertWsSessionCleanup(t, service, session, topic)
	assertWsSessionSendClosed(t, session)
	if got := logs.count([]byte(`"connId":"concurrent-close"`)); got != 1 {
		t.Fatalf("disconnect cleanup records = %d, want exactly 1", got)
	}
}

type wsSessionConcurrencyLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *wsSessionConcurrencyLogBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(payload)
}

func (b *wsSessionConcurrencyLogBuffer) count(pattern []byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Count(b.buffer.Bytes(), pattern)
}

func TestWsSessionQueueFullClose(t *testing.T) {
	service, session, topic := newWsSessionConcurrencyFixture("queue-full-close")
	for i := 0; i < 64; i++ {
		if !session.enqueue([]byte("queued")) {
			t.Fatalf("enqueue entry %d = false, want true", i)
		}
	}
	if session.enqueue([]byte("overflow")) {
		t.Fatal("enqueue on a full 64-entry queue = true, want false")
	}
	runWsSessionConcurrencyWorkers(t, []func(){func() { service.closeSession(session) }})
	if got := len(session.Send); got != 64 {
		t.Fatalf("queued payloads after close = %d, want 64", got)
	}
	assertWsSessionCleanup(t, service, session, topic)
	assertWsSessionSendClosed(t, session)
}

func TestWsSessionEnqueueAfterClose(t *testing.T) {
	service, session, topic := newWsSessionConcurrencyFixture("enqueue-after-close")
	if !session.enqueue([]byte("already queued")) {
		t.Fatal("enqueue before close = false, want true")
	}
	runWsSessionConcurrencyWorkers(t, []func(){func() { service.closeSession(session) }})
	for attempt := 0; attempt < 128; attempt++ {
		if session.enqueue([]byte("after close")) {
			t.Fatalf("post-close enqueue attempt %d = true, want false", attempt)
		}
		if got := len(session.Send); got != 1 {
			t.Fatalf("queue length after post-close enqueue = %d, want 1", got)
		}
	}
	if got := <-session.Send; !bytes.Equal(got, []byte("already queued")) {
		t.Fatalf("queued payload after post-close enqueue = %q, want already queued", got)
	}
	assertWsSessionCleanup(t, service, session, topic)
	assertWsSessionSendClosed(t, session)
}

func TestWsSessionCloseRejectsStaleOperations(t *testing.T) {
	service, session, topic := newWsSessionConcurrencyFixture("stale-operations")
	staleTargets := service.manager.FindDeliveries([]string{topic})
	if len(staleTargets) != 1 || staleTargets[0].Session != session {
		t.Fatalf("captured delivery targets = %v, want session %p", staleTargets, session)
	}

	service.closeSession(session)
	if staleTargets[0].Session.enqueue([]byte("stale delivery")) {
		t.Fatal("enqueue through stale delivery target after close = true, want false")
	}

	lateTopic := "user:late-subscribe"
	got := service.manager.Subscribe(session, []string{lateTopic})
	if _, exists := session.Topics[lateTopic]; exists {
		t.Fatal("late Subscribe added topic to session")
	}
	if service.manager.HasTopic(lateTopic) {
		t.Fatal("late Subscribe added session to topic registry")
	}
	service.manager.mu.RLock()
	_, registered := service.manager.sessions[session.ID]
	service.manager.mu.RUnlock()
	if registered {
		t.Fatal("late Subscribe re-registered managed session")
	}
	if len(got) != 0 {
		t.Fatalf("late Subscribe acknowledgments = %v, want none", got)
	}
}

func TestWsSessionSubscribeBeforeCloseIsUnregistered(t *testing.T) {
	service := newWsService()
	session := &ClientSession{
		ID:     "subscribe-before-close",
		Topics: make(map[string]struct{}),
		Send:   make(chan []byte, realtimeSendBufferSize),
	}
	service.manager.Register(session, nil)
	topic := "user:before-close"
	if got := service.manager.Subscribe(session, []string{topic}); len(got) != 1 || got[0] != topic {
		t.Fatalf("Subscribe acknowledgments = %v, want [%s]", got, topic)
	}

	service.closeSession(session)
	assertWsSessionCleanup(t, service, session, topic)
	if _, exists := session.Topics[topic]; exists {
		t.Fatal("session topic remains after close")
	}
}

func newWsSessionConcurrencyFixture(id string) (*wsService, *ClientSession, string) {
	service := newWsService()
	session := &ClientSession{
		ID:     id,
		Topics: make(map[string]struct{}),
		Send:   make(chan []byte, realtimeSendBufferSize),
	}
	topic := "user:concurrency"
	service.manager.Register(session, []string{topic})
	return service, session, topic
}

func runWsSessionConcurrencyWorkers(t *testing.T, workers []func()) {
	t.Helper()
	start := make(chan struct{})
	outcomes := make(chan any, len(workers))
	for _, work := range workers {
		go func(work func()) {
			defer func() { outcomes <- recover() }()
			<-start
			work()
		}(work)
	}
	close(start)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range workers {
		select {
		case panicValue := <-outcomes:
			if panicValue != nil {
				t.Errorf("session worker panicked: %v", panicValue)
			}
		case <-timer.C:
			t.Fatal("session workers did not complete within five seconds")
		}
	}
}

func assertWsSessionCleanup(t *testing.T, service *wsService, session *ClientSession, topic string) {
	t.Helper()
	if !session.Closed.Load() {
		t.Error("session remains open after closeSession")
	}
	service.manager.mu.RLock()
	_, registered := service.manager.sessions[session.ID]
	service.manager.mu.RUnlock()
	if registered {
		t.Error("session remains registered after closeSession")
	}
	if service.manager.HasTopic(topic) {
		t.Error("session topic remains registered after closeSession")
	}
}

func assertWsSessionSendClosed(t *testing.T, session *ClientSession) {
	t.Helper()
	for {
		select {
		case _, ok := <-session.Send:
			if !ok {
				return
			}
		default:
			t.Fatal("Send remains open after closeSession")
		}
	}
}
