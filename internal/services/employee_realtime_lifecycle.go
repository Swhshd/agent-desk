package services

import (
	"time"

	"agent-desk/internal/pkg/dto"
)

type EmployeeSessionSnapshot struct {
	EmployeeID            int64
	LoginSessionID        int64
	LoginSessionExpiresAt time.Time
	Principal             *dto.AuthPrincipal
	NextAuthzChangeAt     *time.Time
}
type EmployeeSessionRevalidator interface {
	RevalidateEmployeeSession(loginSessionID int64, now time.Time) (EmployeeSessionSnapshot, error)
}

// EmployeeRealtimeInvalidator targets process-local employee sockets only.
type EmployeeRealtimeInvalidator interface {
	InvalidateLoginSession(loginSessionID int64) int
	InvalidateEmployees(employeeIDs []int64) int
	InvalidateAllEmployees() int
}
type EmployeeSessionLifecycle interface {
	EmployeeSessionRevalidator
	InstallLifecycleTimer(session *ClientSession, deadline time.Time) bool
}
type employeeRealtimeLifecycle struct {
	manager   *WsConnectionManager
	reader    employeeAuthStateReader
	now       func() time.Time
	afterFunc func(time.Duration, func()) *time.Timer
}

func newEmployeeRealtimeLifecycle(manager *WsConnectionManager, reader employeeAuthStateReader) *employeeRealtimeLifecycle {
	return &employeeRealtimeLifecycle{manager: manager, reader: reader, now: time.Now, afterFunc: time.AfterFunc}
}

func (l *employeeRealtimeLifecycle) RevalidateEmployeeSession(id int64, now time.Time) (EmployeeSessionSnapshot, error) {
	return l.reader.ReadEmployeeSession(id, now)
}

func employeeLifecycleDeadline(snapshot EmployeeSessionSnapshot) time.Time {
	deadline := snapshot.LoginSessionExpiresAt
	if snapshot.NextAuthzChangeAt != nil && snapshot.NextAuthzChangeAt.Before(deadline) {
		deadline = *snapshot.NextAuthzChangeAt
	}
	return deadline
}

func isEmployeeRealtimeSession(s *ClientSession) bool {
	if s == nil || s.External != nil || s.CustomerID != 0 {
		return false
	}
	return s.Role == realtimeRoleAdmin || s.Role == realtimeRoleNotification || (s.Role == realtimeRoleUser && s.Principal != nil)
}

func (l *employeeRealtimeLifecycle) InstallLifecycleTimer(s *ClientSession, deadline time.Time) bool {
	return l.installLifecycleTimerWithBeforePublish(s, deadline, nil)
}

func (l *employeeRealtimeLifecycle) installLifecycleTimerWithBeforePublish(s *ClientSession, deadline time.Time, beforePublish func()) bool {
	if s == nil {
		return false
	}
	now := l.now()
	if !deadline.After(now) {
		l.manager.CloseSession(s)
		return false
	}
	if s.Closed.Load() {
		return false
	}
	// Keep the local timer unpublished until terminal state is checked again.
	timer := l.afterFunc(deadline.Sub(now), func() { l.manager.CloseSession(s) })
	if beforePublish != nil {
		beforePublish()
	}
	s.lifecycleTimerMu.Lock()
	expired := !deadline.After(l.now())
	installed := !s.Closed.Load() && !expired && s.lifecycleTimer == nil
	if installed {
		s.lifecycleTimer = timer
	}
	s.lifecycleTimerMu.Unlock()
	// Stop never waits for a running callback; callback close uses closeOnce.
	if !installed {
		timer.Stop()
		if expired {
			l.manager.CloseSession(s)
		}
	}
	return installed
}

func (s *ClientSession) detachLifecycleTimer() *time.Timer {
	s.lifecycleTimerMu.Lock()
	timer := s.lifecycleTimer
	s.lifecycleTimer = nil
	s.lifecycleTimerMu.Unlock()
	return timer
}

func (l *employeeRealtimeLifecycle) InvalidateLoginSession(id int64) int {
	if id <= 0 {
		return 0
	}
	return l.closeTargets(l.manager.snapshotEmployeeSessions(id, nil, false))
}

func (l *employeeRealtimeLifecycle) InvalidateEmployees(ids []int64) int {
	targets := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id > 0 {
			targets[id] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return 0
	}
	return l.closeTargets(l.manager.snapshotEmployeeSessions(0, targets, false))
}

func (l *employeeRealtimeLifecycle) InvalidateAllEmployees() int {
	return l.closeTargets(l.manager.snapshotEmployeeSessions(0, nil, true))
}

func (l *employeeRealtimeLifecycle) closeTargets(sessions []*ClientSession) int {
	count := 0
	for _, s := range sessions {
		if l.manager.CloseSession(s) {
			count++
		}
	}
	return count
}
