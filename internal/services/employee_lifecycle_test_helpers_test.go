package services

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-desk/internal/pkg/dto"

	"github.com/gin-gonic/gin"
	"github.com/mlogclub/simple/sqls"
	"gorm.io/gorm"
)

// Isolated fixtures retain the production timer and close path. Only the auth
// read is synthetic; each handshake must explicitly bind its trusted snapshot.
type employeeWsTestLifecycle struct {
	*employeeRealtimeLifecycle
	mu        sync.Mutex
	snapshots map[int64]EmployeeSessionSnapshot
}

func newWsServiceForTest() *wsService {
	manager := newWsConnectionManager()
	lifecycle := &employeeWsTestLifecycle{employeeRealtimeLifecycle: newEmployeeRealtimeLifecycle(manager, employeeAuthStateReader{}), snapshots: make(map[int64]EmployeeSessionSnapshot)}
	return newWsService(manager, lifecycle)
}

func (l *employeeWsTestLifecycle) RevalidateEmployeeSession(id int64, _ time.Time) (EmployeeSessionSnapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot, ok := l.snapshots[id]
	if !ok {
		return EmployeeSessionSnapshot{}, fmt.Errorf("unbound synthetic employee session")
	}
	return snapshot, nil
}

func bindEmployeeWsTestSession(t *testing.T, svc *wsService, ctx *gin.Context, principal *dto.AuthPrincipal) {
	t.Helper()
	lifecycle, ok := svc.lifecycle.(*employeeWsTestLifecycle)
	if !ok || principal == nil || principal.UserID <= 0 {
		t.Fatal("synthetic binding requires test lifecycle and positive principal")
	}
	snapshot := EmployeeSessionSnapshot{EmployeeID: principal.UserID, LoginSessionID: principal.UserID + 1000000, LoginSessionExpiresAt: time.Now().Add(time.Hour), Principal: principal}
	lifecycle.mu.Lock()
	lifecycle.snapshots[snapshot.LoginSessionID] = snapshot
	lifecycle.mu.Unlock()
	ctx.Set(authPrincipalContextKey, principal)
	AuthService.setAuthenticatedEmployeeSession(ctx, authenticatedEmployeeSession{EmployeeID: snapshot.EmployeeID, LoginSessionID: snapshot.LoginSessionID, LoginSessionExpiresAt: snapshot.LoginSessionExpiresAt, Principal: principal})
}

func markActiveEmployeeTestSession(session *ClientSession) {
	if !isEmployeeRealtimeSession(session) {
		return
	}
	if session.Principal != nil {
		session.EmployeeID = session.Principal.UserID
	}
	session.LoginSessionID = session.EmployeeID + 1000000
	session.LoginSessionExpiresAt = time.Now().Add(time.Hour)
	session.LifecycleDeadline = session.LoginSessionExpiresAt
	session.employeePhase.Store(uint32(employeePhaseActive))
}

func lifecycleAuthTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := sqls.DB()
	db := setupAuthServiceTestDB(t)
	t.Cleanup(func() {
		sqls.SetDB(previousDB)
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}
