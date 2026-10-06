package services

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"
	"time"
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
