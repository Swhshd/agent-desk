package services

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-desk/internal/models"
	"agent-desk/internal/pkg/config"
	"agent-desk/internal/pkg/enums"
	"agent-desk/internal/pkg/openidentity"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/mlogclub/simple/sqls"
)

func TestCustomerSessionVerifyRequestReturnsCustomerID(t *testing.T) {
	channel, customerA, _, externalA, _ := setupCustomerSessionIdentityTest(t)
	ctx := signedCustomerSessionTestContext(t, channel, customerA, externalA)
	result, err := CustomerSessionService.VerifyRequest(ctx, channel)
	if err != nil {
		t.Fatalf("VerifyRequest rejected a valid session: %v", err)
	}
	if result == nil || result.CustomerID != 101 {
		t.Fatal("VerifyRequest must return Customer A's verified positive ID")
	}
}

func TestCustomerSessionVerifyRequestRejectsIdentityMappingMismatch(t *testing.T) {
	channel, customerA, _, _, externalB := setupCustomerSessionIdentityTest(t)
	ctx := signedCustomerSessionTestContext(t, channel, customerA, externalB)
	result, err := CustomerSessionService.VerifyRequest(ctx, channel)
	if err == nil || result != nil {
		t.Fatal("VerifyRequest must reject a signed A claim whose identity maps to B")
	}
}

func TestUpgradeConnectionUsesVerifiedCustomerIDNotClientQuery(t *testing.T) {
	channel, customerA, _, externalA, _ := setupCustomerSessionIdentityTest(t)
	ctx := signedCustomerSessionTestContext(t, channel, customerA, externalA)
	verified, err := CustomerSessionService.VerifyRequest(ctx, channel)
	if err != nil {
		t.Fatalf("verify synthetic session: %v", err)
	}

	svc := newWsService()
	upgraded := make(chan error, 1)
	router := gin.New()
	router.GET("/ws", func(ctx *gin.Context) {
		upgraded <- svc.upgradeConnection(ctx, nil, verified.ExternalUser, realtimeRoleUser, verified)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws?customerId=202", nil)
	if err != nil {
		t.Fatalf("websocket handshake: %v", err)
	}
	defer conn.Close()
	select {
	case err := <-upgraded:
		if err != nil {
			t.Fatalf("upgradeConnection: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upgradeConnection did not finish")
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read connected frame: %v", err)
	}
	var connected struct {
		Type string `json:"type"`
		Data struct {
			ConnID string `json:"connId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &connected); err != nil {
		t.Fatal("decode connected frame")
	}
	if connected.Type != enums.IMRealtimeEventConnected || connected.Data.ConnID == "" {
		t.Fatal("expected connected frame with connection ID")
	}
	svc.manager.mu.RLock()
	session := svc.manager.sessions[connected.Data.ConnID]
	svc.manager.mu.RUnlock()
	if session == nil {
		t.Fatal("upgraded session was not registered")
	}
	defer svc.closeSession(session)
	if session.CustomerID != 101 {
		t.Fatalf("registered CustomerID = %d, want verified A (101), despite query B (202)", session.CustomerID)
	}
}

func setupCustomerSessionIdentityTest(t *testing.T) (*models.Channel, *models.Customer, *models.Customer, openidentity.ExternalUser, openidentity.ExternalUser) {
	t.Helper()
	previousDB := sqls.DB()
	previousConfig := config.GetCurrent()
	t.Cleanup(func() {
		sqls.SetDB(previousDB)
		config.SetCurrent(previousConfig)
	})
	db := openHumanDispatchRealtimeTestDB(t, false)
	signingKey := make([]byte, 32)
	if _, err := rand.Read(signingKey); err != nil {
		t.Fatal("generate in-memory signing key")
	}
	config.SetCurrent(&config.Config{CustomerSession: config.CustomerSessionConfig{
		Secret: base64.RawURLEncoding.EncodeToString(signingKey),
	}})
	channel := &models.Channel{ID: 301, ChannelID: "synthetic-session-channel", Status: enums.StatusOk}
	customerA := &models.Customer{ID: 101, Name: "Synthetic Customer A", Status: enums.StatusOk}
	customerB := &models.Customer{ID: 202, Name: "Synthetic Customer B", Status: enums.StatusOk}
	externalA := openidentity.ExternalUser{ExternalSource: enums.ExternalSourceGuest, ExternalID: "synthetic-guest-a"}
	externalB := openidentity.ExternalUser{ExternalSource: enums.ExternalSourceGuest, ExternalID: "synthetic-guest-b"}
	for _, row := range []any{
		channel, customerA, customerB,
		&models.CustomerIdentity{CustomerID: 101, ExternalSource: externalA.ExternalSource, ExternalID: externalA.ExternalID, Status: enums.StatusOk},
		&models.CustomerIdentity{CustomerID: 202, ExternalSource: externalB.ExternalSource, ExternalID: externalB.ExternalID, Status: enums.StatusOk},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("create synthetic session fixture: %v", err)
		}
	}
	return channel, customerA, customerB, externalA, externalB
}

func signedCustomerSessionTestContext(t *testing.T, channel *models.Channel, customer *models.Customer, external openidentity.ExternalUser) *gin.Context {
	t.Helper()
	token, _, err := CustomerSessionService.Sign(channel, customer, external)
	if err != nil {
		t.Fatal("sign synthetic customer session")
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/ws", nil)
	ctx.Request.Header.Set("Authorization", "Bearer "+token)
	return ctx
}
