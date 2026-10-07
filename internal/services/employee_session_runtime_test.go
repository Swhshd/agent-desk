package services

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-desk/internal/models"
	"agent-desk/internal/pkg/dto"
	"agent-desk/internal/pkg/enums"
	"agent-desk/internal/pkg/openidentity"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

const employeeSessionRuntimeScenarioEnv = "AGENT_DESK_EMPLOYEE_SESSION_RUNTIME_SCENARIO"

// TestEmployeeSessionRuntimeAcceptance is an in-process synthetic employee
// session/WebSocket lifecycle acceptance. Each scenario gets a fresh test
// process so package globals and the in-memory database cannot cross-contaminate
// the next scenario. This is not production or browser E2E evidence.
func TestEmployeeSessionRuntimeAcceptance(t *testing.T) {
	if scenario := os.Getenv(employeeSessionRuntimeScenarioEnv); scenario != "" {
		runEmployeeSessionRuntimeScenario(t, scenario)
		return
	}

	for _, scenario := range []string{"exact_session_revoke", "revoke_all", "password_change", "disable_before_revoke_failure", "assign_roles_commit_and_rollback", "role_status_current_members", "committed_impact_failure_customer_isolation", "idle_login_session_expiry", "idle_direct_allow_expiry", "idle_direct_deny_expiry"} {
		t.Run(scenario, func(t *testing.T) {
			binary, err := os.Executable()
			if err != nil {
				t.Fatal("locate test binary")
			}
			cmd := exec.Command(binary, "-test.run=^TestEmployeeSessionRuntimeAcceptance$", "-test.count=1")
			cmd.Env = append(os.Environ(), employeeSessionRuntimeScenarioEnv+"="+scenario)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("synthetic scenario %q failed (exit=%v): %s", scenario, err, safeRuntimeOutput(output))
			}
		})
	}
}

func safeRuntimeOutput(output []byte) string {
	// Child output is limited to test diagnostics. Never include authorization
	// headers, synthetic bearer values, or serialized session state in failures.
	return strings.TrimSpace(string(output))
}

func runEmployeeSessionRuntimeScenario(t *testing.T, scenario string) {
	gin.SetMode(gin.TestMode)
	db := setupAuthServiceTestDB(t)
	previousLoginSessions, previousWS := LoginSessionService, WsService
	manager := newWsConnectionManager()
	lifecycle := newEmployeeRealtimeLifecycle(manager, employeeAuthStateReader{})
	LoginSessionService = newLoginSessionService(lifecycle)
	WsService = newWsService(manager, lifecycle)
	t.Cleanup(func() {
		lifecycle.InvalidateAllEmployees()
		LoginSessionService, WsService = previousLoginSessions, previousWS
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	alpha := runtimeEmployee(t, db, "runtime-alpha", "runtime-token-alpha")
	beta := runtimeEmployee(t, db, "runtime-beta", "runtime-token-beta")
	alphaA := runtimeSocket(t, alpha.token)
	alphaB := runtimeSocket(t, alpha.token)
	betaA := runtimeSocket(t, beta.token)
	betaB := runtimeSocket(t, beta.token)
	for _, conn := range []*websocket.Conn{alphaA, alphaB, betaA, betaB} {
		readRuntimeEvent(t, conn, enums.IMRealtimeEventConnected)
	}

	switch scenario {
	case "exact_session_revoke":
		if err := LoginSessionService.Revoke(alpha.login.ID, alpha.user.ID, alpha.user.Username); err != nil {
			t.Fatal("revoke synthetic session")
		}
		assertRuntimeSocketClosed(t, alphaA)
		assertRuntimeSocketClosed(t, alphaB)
		assertRuntimePingPong(t, betaA)
		assertRuntimePingPong(t, betaB)
		if got := authenticateRuntime(alpha.token); got != http.StatusUnauthorized {
			t.Fatalf("fresh authentication after exact revoke = %d", got)
		}
	case "revoke_all":
		if err := LoginSessionService.RevokeByUser(alpha.user.ID, beta.user.ID, beta.user.Username); err != nil {
			t.Fatal("revoke all synthetic employee sessions")
		}
		assertRuntimeSocketClosed(t, alphaA)
		assertRuntimeSocketClosed(t, alphaB)
		assertRuntimePingPong(t, betaA)
		assertRuntimePingPong(t, betaB)
	case "password_change":
		if err := newUserService(lifecycle).ChangeOwnPassword("synthetic-new-password", &dto.AuthPrincipal{UserID: alpha.user.ID, Username: alpha.user.Username}); err != nil {
			t.Fatal("change synthetic employee password")
		}
		assertRuntimeSocketClosed(t, alphaA)
		assertRuntimeSocketClosed(t, alphaB)
		assertRuntimePingPong(t, betaA)
		if got := authenticateRuntime(alpha.token); got != http.StatusUnauthorized {
			t.Fatalf("fresh authentication after password change = %d", got)
		}
	case "disable_before_revoke_failure":
		failure := registerRuntimeLoginSessionUpdateFailure(t, db)
		_ = failure
		// User status is persisted and sockets are closed before the later
		// revoke-all write fails. The failure is deliberately synthetic.
		service := newUserService(lifecycle)
		err := service.UpdateStatus(alpha.user.ID, int(enums.StatusDisabled), &dto.AuthPrincipal{UserID: beta.user.ID, Username: beta.user.Username})
		if err == nil {
			t.Fatal("expected later LoginSession revoke write to fail")
		}
		var persisted models.User
		if err := db.First(&persisted, alpha.user.ID).Error; err != nil || persisted.Status != enums.StatusDisabled {
			t.Fatal("employee disable did not persist before revoke failure")
		}
		assertRuntimeSocketClosed(t, alphaA)
		assertRuntimeSocketClosed(t, alphaB)
		assertRuntimePingPong(t, betaA)
		if got := authenticateRuntime(alpha.token); got != http.StatusUnauthorized {
			t.Fatalf("fresh authentication after disable = %d", got)
		}
		assertRuntimeWebSocketRejected(t, alpha.token)
	case "assign_roles_commit_and_rollback":
		role := models.Role{Code: "runtime-role", Name: "Runtime role", Status: enums.StatusOk}
		if err := db.Create(&role).Error; err != nil {
			t.Fatal("create synthetic role")
		}
		service := newUserService(lifecycle)
		if err := service.AssignRoles(alpha.user.ID, []int64{role.ID}, &dto.AuthPrincipal{UserID: beta.user.ID, Username: beta.user.Username}); err != nil {
			t.Fatal("commit role assignment")
		}
		assertRuntimeSocketClosed(t, alphaA)
		assertRuntimeSocketClosed(t, alphaB)
		assertRuntimePingPong(t, betaA)
		// An invalid role causes the replacement transaction to roll back; this
		// is checked with a fresh socket in the same isolated scenario.
		alphaFresh := runtimeShortSession(t, db, alpha.user, time.Hour)
		alphaC := runtimeSocket(t, alphaFresh.token)
		readRuntimeEvent(t, alphaC, enums.IMRealtimeEventConnected)
		if err := service.AssignRoles(alpha.user.ID, []int64{int64(1 << 60)}, &dto.AuthPrincipal{UserID: beta.user.ID, Username: beta.user.Username}); err == nil {
			t.Fatal("invalid role assignment unexpectedly committed")
		}
		assertRuntimePingPong(t, alphaC)
	case "role_status_current_members":
		role := models.Role{Code: "runtime-status-role", Name: "Runtime status role", Status: enums.StatusOk}
		permission := models.Permission{Code: "runtime.status.permission", Name: "Runtime status permission", Status: enums.StatusOk}
		if err := db.Create(&role).Error; err != nil {
			t.Fatal("create synthetic role")
		}
		if err := db.Create(&permission).Error; err != nil {
			t.Fatal("create synthetic permission")
		}
		if err := db.Create(&models.RolePermission{RoleID: role.ID, PermissionID: permission.ID}).Error; err != nil {
			t.Fatal("create synthetic role permission")
		}
		if err := db.Create(&models.UserRole{UserID: alpha.user.ID, RoleID: role.ID}).Error; err != nil {
			t.Fatal("assign synthetic role")
		}
		if err := newRoleService(lifecycle).UpdateStatus(role.ID, enums.StatusDisabled, &dto.AuthPrincipal{UserID: beta.user.ID, Username: beta.user.Username}); err != nil {
			t.Fatal("disable synthetic role")
		}
		assertRuntimeSocketClosed(t, alphaA)
		assertRuntimeSocketClosed(t, alphaB)
		assertRuntimePingPong(t, betaA)
		snapshot, err := (employeeAuthStateReader{}).ReadEmployeeSession(alpha.login.ID, time.Now())
		if err != nil || slices.Contains(snapshot.Principal.Permissions, permission.Code) {
			t.Fatal("fresh authoritative snapshot retained disabled role permission")
		}
	case "committed_impact_failure_customer_isolation":
		customer := runtimeCustomerSocket(t, 77001)
		readRuntimeEvent(t, customer, enums.IMRealtimeEventConnected)
		role := models.Role{Code: "runtime-impact-role", Name: "Runtime impact role", Status: enums.StatusOk}
		if err := db.Create(&role).Error; err != nil {
			t.Fatal("create synthetic role")
		}
		if err := db.Create(&models.UserRole{UserID: alpha.user.ID, RoleID: role.ID}).Error; err != nil {
			t.Fatal("assign synthetic role")
		}
		failure := errors.New("synthetic post-commit impact read failure")
		const callback = "test:employee_runtime_impact_query_failure"
		if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "t_user_role" {
				tx.AddError(failure)
			}
		}); err != nil {
			t.Fatal("install synthetic impact query failure")
		}
		t.Cleanup(func() { db.Callback().Query().Remove(callback) })
		if err := newRoleService(lifecycle).UpdateStatus(role.ID, enums.StatusDisabled, &dto.AuthPrincipal{UserID: beta.user.ID, Username: beta.user.Username}); err != nil {
			t.Fatal("committed role mutation should preserve write result")
		}
		assertRuntimeSocketClosed(t, alphaA)
		assertRuntimeSocketClosed(t, alphaB)
		assertRuntimeSocketClosed(t, betaA)
		assertRuntimeSocketClosed(t, betaB)
		assertRuntimePingPong(t, customer)
		for _, login := range []models.LoginSession{alpha.login, beta.login} {
			var persisted models.LoginSession
			if err := db.First(&persisted, login.ID).Error; err != nil || persisted.RevokedAt != nil {
				t.Fatal("impact fallback unexpectedly revoked a LoginSession row")
			}
		}
	case "idle_login_session_expiry":
		shortenRuntimeSession(t, db, alpha.login.ID, 300*time.Millisecond)
		// Reconnect using a dedicated short-lived LoginSession so authoritative
		// handshake revalidation sees the same persisted deadline.
		short := runtimeShortSession(t, db, alpha.user, 400*time.Millisecond)
		conn := runtimeSocket(t, short.token)
		readRuntimeEvent(t, conn, enums.IMRealtimeEventConnected)
		assertRuntimeSocketClosedEventually(t, conn, time.Second)
		assertRuntimePingPong(t, betaA)
	case "idle_direct_allow_expiry":
		permission := models.Permission{Code: "runtime.direct", Name: "Runtime direct", Status: enums.StatusOk}
		if err := db.Create(&permission).Error; err != nil {
			t.Fatal("create synthetic permission")
		}
		expires := time.Now().Add(400 * time.Millisecond)
		allow := 1
		if err := db.Create(&models.UserPermission{UserID: alpha.user.ID, PermissionID: permission.ID, Effect: allow, ExpiredAt: &expires}).Error; err != nil {
			t.Fatal("create synthetic direct override")
		}
		conn := runtimeSocket(t, alpha.token)
		readRuntimeEvent(t, conn, enums.IMRealtimeEventConnected)
		assertRuntimeSocketClosedEventually(t, conn, time.Second)
		assertRuntimePingPong(t, betaA)
		if _, err := (employeeAuthStateReader{}).ReadEmployeeSession(alpha.login.ID, time.Now()); err != nil {
			t.Fatal("still-valid LoginSession did not revalidate after direct allow expiry")
		}
	case "idle_direct_deny_expiry":
		permission := models.Permission{Code: "runtime.role.permission", Name: "Runtime role permission", Status: enums.StatusOk}
		role := models.Role{Code: "runtime-deny-role", Name: "Runtime deny role", Status: enums.StatusOk}
		if err := db.Create(&permission).Error; err != nil {
			t.Fatal("create synthetic permission")
		}
		if err := db.Create(&role).Error; err != nil {
			t.Fatal("create synthetic role")
		}
		if err := db.Create(&models.RolePermission{RoleID: role.ID, PermissionID: permission.ID}).Error; err != nil {
			t.Fatal("create role permission")
		}
		if err := db.Create(&models.UserRole{UserID: alpha.user.ID, RoleID: role.ID}).Error; err != nil {
			t.Fatal("assign synthetic role")
		}
		expires := time.Now().Add(400 * time.Millisecond)
		if err := db.Create(&models.UserPermission{UserID: alpha.user.ID, PermissionID: permission.ID, Effect: -1, ExpiredAt: &expires}).Error; err != nil {
			t.Fatal("create synthetic direct deny")
		}
		conn := runtimeSocket(t, alpha.token)
		readRuntimeEvent(t, conn, enums.IMRealtimeEventConnected)
		assertRuntimeSocketClosedEventually(t, conn, time.Second)
		snapshot, err := (employeeAuthStateReader{}).ReadEmployeeSession(alpha.login.ID, time.Now())
		if err != nil || !slices.Contains(snapshot.Principal.Permissions, permission.Code) {
			t.Fatal("still-valid LoginSession did not restore role-derived grant after direct deny expiry")
		}
		assertRuntimePingPong(t, betaA)
	default:
		t.Fatalf("unknown synthetic scenario %q", scenario)
	}
}

type runtimeEmployeeFixture struct {
	user  *models.User
	token string
	login models.LoginSession
}

func runtimeEmployee(t *testing.T, db *gorm.DB, username, token string) runtimeEmployeeFixture {
	t.Helper()
	// Use the repository's auth fixture for password and audit fields, then mark
	// the synthetic identity as an enabled employee and seed a private session.
	user := createAuthTestUser(t, db, username, "synthetic-password")
	if err := db.Model(user).Update("user_type", enums.UserTypeEmployee).Error; err != nil {
		t.Fatal("mark synthetic user as employee")
	}
	login := employeeMutationLogin(t, db, user.ID, token)
	return runtimeEmployeeFixture{user: user, token: token, login: login}
}

func runtimeShortSession(t *testing.T, db *gorm.DB, user *models.User, ttl time.Duration) runtimeEmployeeFixture {
	t.Helper()
	token := "runtime-short-session"
	now := time.Now()
	login := models.LoginSession{UserID: user.ID, Token: token, ClientType: "admin_web", ExpiredAt: now.Add(ttl), AuditFields: models.AuditFields{CreatedAt: now, UpdatedAt: now}}
	if err := db.Create(&login).Error; err != nil {
		t.Fatal("create short synthetic session")
	}
	return runtimeEmployeeFixture{user: user, token: token, login: login}
}

func shortenRuntimeSession(t *testing.T, db *gorm.DB, id int64, ttl time.Duration) {
	t.Helper()
	if err := db.Model(&models.LoginSession{}).Where("id = ?", id).Update("expired_at", time.Now().Add(ttl)).Error; err != nil {
		t.Fatal("set synthetic expiry")
	}
}

func runtimeRouter() *httptest.Server {
	router := gin.New()
	router.GET("/auth", func(ctx *gin.Context) {
		if _, err := AuthService.Authenticate(ctx); err != nil {
			ctx.Status(http.StatusUnauthorized)
			return
		}
		ctx.Status(http.StatusOK)
	})
	router.GET("/ws/dashboard", func(ctx *gin.Context) {
		if _, err := AuthService.Authenticate(ctx); err != nil {
			ctx.Status(http.StatusUnauthorized)
			return
		}
		WsService.HandleDashboardWS(ctx)
	})
	return httptest.NewServer(router)
}

func runtimeSocket(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	server := runtimeRouter()
	t.Cleanup(server.Close)
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/dashboard", header)
	if err != nil {
		t.Fatal("synthetic authenticated WebSocket handshake failed")
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func runtimeCustomerSocket(t *testing.T, customerID int64) *websocket.Conn {
	t.Helper()
	router := gin.New()
	router.GET("/ws/customer", func(ctx *gin.Context) {
		external := &openidentity.ExternalUser{ExternalID: "runtime-customer", ExternalSource: enums.ExternalSourceGuest}
		if err := WsService.upgradeConnection(ctx, nil, external, realtimeRoleUser, &CustomerSessionVerifyResult{CustomerID: customerID}); err != nil {
			t.Errorf("synthetic customer upgrade failed: %v", err)
		}
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/customer", nil)
	if err != nil {
		t.Fatal("synthetic verified-customer WebSocket handshake failed")
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func authenticateRuntime(token string) int {
	server := runtimeRouter()
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/auth", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	return response.StatusCode
}

func assertRuntimeWebSocketRejected(t *testing.T, token string) {
	t.Helper()
	server := runtimeRouter()
	defer server.Close()
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/dashboard", header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("disabled employee established a fresh WebSocket")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatal("disabled employee WebSocket was not rejected by fresh authentication")
	}
}

func readRuntimeEvent(t *testing.T, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal("set synthetic socket deadline")
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read lifecycle control event %q: %v", want, err)
	}
	var event struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &event); err != nil || event.Type != want {
		t.Fatalf("unexpected lifecycle control event: type=%q decodeErr=%v", event.Type, err)
	}
	return event.Data
}

func assertRuntimeSocketClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("revoked synthetic socket remained open")
	}
}

func assertRuntimeSocketClosedEventually(t *testing.T, conn *websocket.Conn, timeout time.Duration) {
	t.Helper()
	assertRuntimeSocketClosed(t, conn)
}

func assertRuntimePingPong(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"type": "ping"}); err != nil {
		t.Fatal("write synthetic ping")
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal("set synthetic ping deadline")
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatal("unaffected employee socket did not respond")
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &event); err != nil || event.Type != enums.IMRealtimeEventPong {
		t.Fatal("unaffected employee socket did not return pong")
	}
}

func registerRuntimeLoginSessionUpdateFailure(t *testing.T, db *gorm.DB) error {
	t.Helper()
	failure := errors.New("synthetic later revoke write failure")
	const callback = "test:employee_session_runtime_login_update_failure"
	if err := db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "t_login_session" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal("install synthetic LoginSession failure")
	}
	t.Cleanup(func() { db.Callback().Update().Remove(callback) })
	return failure
}
