package services

import (
	"agent-desk/internal/models"
	"agent-desk/internal/pkg/dto"
	"agent-desk/internal/pkg/dto/request"
	"agent-desk/internal/pkg/enums"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// These exercise the actual business entrypoints: removing receipt shaping or
// bypassing the shared fanout boundary exposes private JSON to queue captures.
func TestEmployeeRealtimePublicationFamilies(t *testing.T) {
	families := []string{"create welcome", "customer unassigned", "customer assigned", "employee reply", "AI workflow reply", "AI service notice", "recall", "assign", "transfer", "close", "agent read", "customer read", "dispatch", "assigned handoff", "pool handoff"}
	for _, kind := range []string{enums.IMRealtimeEventConversationCreated, enums.IMRealtimeEventConversationUpdated, enums.IMRealtimeEventConversationAssigned, enums.IMRealtimeEventConversationTransferred, enums.IMRealtimeEventConversationClosed, enums.IMRealtimeEventConversationRead} {
		families = append(families, "general "+kind)
	}
	for _, family := range families {
		t.Run(family, func(t *testing.T) {
			if runEmployeePublicationSubprocess(t) {
				return
			}
			db, svc := setupEmployeePublicationTest(t)
			ai := createWelcomeTestAIAgent(t, db, "")
			if family == "create welcome" {
				if err := db.Model(ai).Update("welcome_message", "synthetic-publication-marker").Error; err != nil {
					t.Fatal(err)
				}
			}
			external, observerExternal := createCustomerPublicationCollision(t, db, "synthetic-"+t.Name())
			queue := captureEmployeeRealtimeSession(t, svc, "queue", employeeViewPrincipal(), "admin:all", "admin:101")
			denied := captureEmployeeRealtimeSession(t, svc, "denied", &dto.AuthPrincipal{UserID: 101}, "admin:all", "admin:101")
			missing := captureEmployeeRealtimeSession(t, svc, "missing", nil, "admin:all", "admin:101")
			customer := captureCustomerRealtimeSession(t, svc, "customer A", 41, &external)
			observer := captureCustomerRealtimeSession(t, svc, "customer B", 42, &observerExternal)
			conv, err := ConversationService.CreateForCustomer(41, external, 11, ai.ID)
			if err != nil {
				t.Fatal(err)
			}
			if conv.CustomerID != 41 {
				t.Fatalf("A conversation owner = %d, want 41", conv.CustomerID)
			}
			requireNoCapturedRealtimeEvent(t, observer)
			if family == "create welcome" {
				for _, kind := range []string{enums.IMRealtimeEventConversationCreated, enums.IMRealtimeEventMessageCreated, enums.IMRealtimeEventConversationUpdated} {
					q := requireCapturedRealtimeEvent(t, queue, kind)
					c := requireCapturedRealtimeEvent(t, customer, kind)
					keys := employeeConversationQueueKeys
					if kind == enums.IMRealtimeEventMessageCreated {
						keys = employeeMessageQueueKeys
						if c.Data["content"] != "synthetic-publication-marker" {
							t.Fatal("welcome content lost")
						}
					}
					assertEmployeeQueueData(t, q, keys)
					if q.EventID != c.EventID {
						t.Fatal("different event IDs")
					}
				}
				requireNoCapturedRealtimeEvent(t, denied)
				requireNoCapturedRealtimeEvent(t, missing)
				requireNoCapturedRealtimeEvent(t, queue)
				requireNoCapturedRealtimeEvent(t, customer)
				captureEmployeeRealtimeSession(t, svc, "full after create", employeeViewPrincipal(), svc.conversationTopic(conv.ID))
				return
			}
			// Creation is synchronous; drain it before capturing the family under test.
			drainEmployeePublication(queue, denied, missing, customer)
			full := captureEmployeeRealtimeSession(t, svc, "full", employeeViewPrincipal(), svc.conversationTopic(conv.ID))
			// Admission proves ownership; later empty-queue checks prove one frame
			// per publication even though A belongs to both delivery destinations.
			if got := svc.subscribeTopics(customer, []string{svc.conversationTopic(conv.ID)}); !reflect.DeepEqual(got, []string{svc.conversationTopic(conv.ID)}) {
				t.Fatalf("customer owner conversation subscription = %v", got)
			}
			svc.manager.Subscribe(denied, []string{svc.conversationTopic(conv.ID)})
			svc.manager.Subscribe(missing, []string{svc.conversationTopic(conv.ID)})
			createEmployeePublicationAgent(t, db, 101, 1)
			operator := &dto.AuthPrincipal{UserID: 101, Username: "synthetic-agent-101"}
			setConversation := func(status enums.IMConversationStatus, assignee int64) {
				t.Helper()
				if err := db.Model(conv).Updates(map[string]any{"status": status, "current_assignee_id": assignee, "last_message_summary": "synthetic-private-summary"}).Error; err != nil {
					t.Fatal(err)
				}
				conv = ConversationService.Get(conv.ID)
			}
			sendCustomer := func() *models.Message {
				t.Helper()
				m, err := MessageService.SendVerifiedCustomerMessageWithRequestID(conv.ID, conv.CustomerID, "synthetic-customer-"+family, enums.IMMessageTypeText, "synthetic-publication-marker", "", external, "synthetic-request")
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			sendAgent := func() *models.Message {
				t.Helper()
				m, err := MessageService.SendAgentMessageWithRequestID(conv.ID, 101, "synthetic-agent-"+family, enums.IMMessageTypeText, "synthetic-publication-marker", "", operator, "synthetic-request")
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			var expected []string
			switch family {
			case "customer unassigned":
				setConversation(enums.IMConversationStatusAIServing, 0)
				sendCustomer()
				expected = []string{enums.IMRealtimeEventMessageCreated, enums.IMRealtimeEventConversationUpdated}
			case "customer assigned":
				setConversation(enums.IMConversationStatusActive, 101)
				sendCustomer()
				expected = []string{enums.IMRealtimeEventMessageCreated, enums.IMRealtimeEventConversationUpdated}
			case "employee reply":
				setConversation(enums.IMConversationStatusActive, 101)
				sendAgent()
				expected = []string{enums.IMRealtimeEventMessageCreated, enums.IMRealtimeEventConversationUpdated}
			case "AI workflow reply":
				setConversation(enums.IMConversationStatusAIServing, 0)
				_, err = MessageService.SendAIMessageWithRequestIDAndWorkflowRunID(conv.ID, ai.ID, "synthetic-ai", enums.IMMessageTypeText, "synthetic-publication-marker", "", workflowTestAIPrincipal(), "synthetic-request", 77)
				expected = []string{enums.IMRealtimeEventMessageCreated, enums.IMRealtimeEventConversationUpdated}
			case "AI service notice":
				setConversation(enums.IMConversationStatusPending, 0)
				if e := db.Model(conv).Update("handoff_at", time.Now()).Error; e != nil {
					t.Fatal(e)
				}
				_, err = MessageService.SendAIServiceNoticeWithRequestID(conv.ID, ai.ID, "synthetic-publication-marker", "synthetic-request")
				expected = []string{enums.IMRealtimeEventMessageCreated, enums.IMRealtimeEventConversationUpdated}
			case "recall":
				setConversation(enums.IMConversationStatusActive, 101)
				message := sendAgent()
				drainEmployeePublication(queue, full, customer, denied, missing)
				_, err = MessageService.RecallAgentMessage(message.ID, operator)
				expected = []string{enums.IMRealtimeEventMessageRecalled, enums.IMRealtimeEventConversationUpdated}
			case "assign":
				setConversation(enums.IMConversationStatusPending, 0)
				err = ConversationService.AssignConversation(request.AssignConversationRequest{ConversationID: conv.ID, AssigneeID: 101, Reason: "synthetic-assign"}, operator)
				expected = []string{enums.IMRealtimeEventConversationAssigned}
			case "transfer":
				setConversation(enums.IMConversationStatusActive, 101)
				createEmployeePublicationAgent(t, db, 201, 1)
				queue.Principal.UserID = 201
				denied.Principal.UserID = 201
				svc.manager.Subscribe(queue, []string{"admin:201"})
				svc.manager.Subscribe(denied, []string{"admin:201"})
				svc.manager.Subscribe(missing, []string{"admin:201"})
				err = ConversationService.TransferConversation(conv.ID, 201, "synthetic-transfer", operator)
				expected = []string{enums.IMRealtimeEventConversationTransferred}
			case "close":
				setConversation(enums.IMConversationStatusActive, 101)
				err = ConversationService.CloseConversation(conv.ID, "synthetic-close", operator)
				expected = []string{enums.IMRealtimeEventConversationClosed}
			case "agent read":
				setConversation(enums.IMConversationStatusActive, 101)
				message := sendCustomer()
				drainEmployeePublication(queue, full, customer, denied, missing)
				err = ConversationService.MarkAgentConversationReadToMessage(conv.ID, message.ID, operator)
				expected = []string{enums.IMRealtimeEventConversationRead}
			case "customer read":
				setConversation(enums.IMConversationStatusActive, 101)
				message := sendAgent()
				drainEmployeePublication(queue, full, customer, denied, missing)
				err = ConversationService.MarkVerifiedCustomerConversationReadToMessage(conv.ID, message.ID, conv.CustomerID, &external)
				expected = []string{enums.IMRealtimeEventConversationRead}
			case "dispatch", "assigned handoff", "pool handoff":
				createHumanDispatchRealtimeTeam(t, db, 1)
				createHumanDispatchRealtimeActiveSchedule(t, db, 1)
				if e := db.Model(ai).Updates(map[string]any{"service_mode": enums.IMConversationServiceModeAIFirst, "team_ids": "1"}).Error; e != nil {
					t.Fatal(e)
				}
				ai = AIAgentService.Get(ai.ID)
				if family == "dispatch" {
					setConversation(enums.IMConversationStatusPending, 0)
					var assigned *models.Conversation
					assigned, err = ConversationDispatchService.DispatchPendingConversation(conv, ai)
					if err == nil && (assigned == nil || assigned.CurrentAssigneeID != 101) {
						t.Fatal("dispatch did not assign 101")
					}
					expected = []string{enums.IMRealtimeEventConversationAssigned}
				} else {
					setConversation(enums.IMConversationStatusAIServing, 0)
					if family == "pool handoff" {
						if e := db.Model(&models.AgentProfile{}).Where("user_id = ?", 101).Update("auto_assign_enabled", false).Error; e != nil {
							t.Fatal(e)
						}
					}
					var result *HandoffDecisionResult
					result, err = ConversationHumanDispatchService.HandoffByAI(conv.ID, *ai, "synthetic-handoff")
					if err == nil && (result == nil || family == "assigned handoff" && result.Decision != HandoffDecisionAssigned || family == "pool handoff" && result.Decision != HandoffDecisionTeamPool) {
						t.Fatalf("unexpected handoff decision: %+v", result)
					}
					if family == "assigned handoff" {
						expected = []string{enums.IMRealtimeEventConversationAssigned}
					} else {
						expected = []string{enums.IMRealtimeEventConversationUpdated}
					}
				}
			default:
				kind := family[len("general "):]
				setConversation(enums.IMConversationStatusActive, 101)
				svc.PublishConversationChanged(conv, kind)
				expected = []string{kind}
			}
			if err != nil {
				t.Fatal(err)
			}
			// Handoff emits preceding state/message events as well; inspect every frame.
			seen := map[string]bool{}
			for len(queue.Send) > 0 {
				var q capturedRealtimeEvent
				if e := json.Unmarshal(<-queue.Send, &q); e != nil {
					t.Fatal(e)
				}
				f := requireCapturedRealtimeEvent(t, full, q.Type)
				c := requireCapturedRealtimeEvent(t, customer, q.Type)
				requireNoCapturedRealtimeEvent(t, observer)
				if !reflect.DeepEqual(f, c) {
					t.Fatalf("customer original differs from full employee: full=%+v customer=%+v", f, c)
				}
				if q.EventID != f.EventID || q.Topic != f.Topic || q.At != f.At {
					t.Fatal("fanout envelope changed")
				}
				keys := employeeConversationQueueKeys
				if q.Type == enums.IMRealtimeEventMessageCreated {
					keys = employeeMessageQueueKeys
					if _, ok := f.Data["message"]; !ok {
						t.Fatal("full nested message lost")
					}
					if family == "AI workflow reply" {
						if f.Data["message"].(map[string]any)["workflowRunId"] != float64(77) {
							t.Fatal("workflow association lost")
						}
					}
					if family != "assigned handoff" && family != "pool handoff" && f.Data["content"] != "synthetic-publication-marker" {
						t.Fatal("full content marker lost")
					}
				}
				if q.Type == enums.IMRealtimeEventMessageRecalled {
					keys = employeeRecallQueueKeys
					if _, ok := f.Data["recalledAt"]; !ok {
						t.Fatal("full recall metadata lost")
					}
				}
				assertEmployeeQueueData(t, q, keys)
				for _, key := range keys {
					if !reflect.DeepEqual(q.Data[key], f.Data[key]) {
						t.Fatalf("queue metadata %s lost: queue=%v full=%v", key, q.Data[key], f.Data[key])
					}
				}
				if family == "agent read" {
					if _, ok := f.Data["agentLastReadMessageId"]; !ok {
						t.Fatal("agent read marker lost")
					}
				}
				if family == "customer read" {
					if _, ok := f.Data["customerLastReadMessageId"]; !ok {
						t.Fatal("customer read marker lost")
					}
				}
				if q.Type == enums.IMRealtimeEventConversationAssigned && q.Data["currentAssigneeId"] != float64(101) {
					t.Fatal("personal assignment lost")
				}
				if family == "transfer" && q.Data["currentAssigneeId"] != float64(201) {
					t.Fatal("new assignee route lost")
				}
				seen[q.Type] = true
			}
			for _, kind := range expected {
				if !seen[kind] {
					t.Fatalf("missing publication %s", kind)
				}
			}
			requireNoCapturedRealtimeEvent(t, full)
			requireNoCapturedRealtimeEvent(t, customer)
			requireNoCapturedRealtimeEvent(t, observer)
			requireNoCapturedRealtimeEvent(t, denied)
			requireNoCapturedRealtimeEvent(t, missing)
			if family == "customer assigned" {
				observerConversation := &models.Conversation{CustomerID: 42, ChannelID: 11, AIAgentID: ai.ID, Status: enums.IMConversationStatusActive, CurrentAssigneeID: 101}
				if err := db.Create(observerConversation).Error; err != nil {
					t.Fatal(err)
				}
				if _, err := MessageService.SendVerifiedCustomerMessageWithRequestID(observerConversation.ID, observerConversation.CustomerID, "synthetic-B-owned-message", enums.IMMessageTypeText, "synthetic-owner-B-marker", "", observerExternal, "synthetic-B-request"); err != nil {
					t.Fatal(err)
				}
				message := requireCapturedRealtimeEvent(t, observer, enums.IMRealtimeEventMessageCreated)
				if message.Data["content"] != "synthetic-owner-B-marker" || message.Data["conversationId"] != float64(observerConversation.ID) {
					t.Fatal("B-owned message marker or conversation lost")
				}
				requireCapturedRealtimeEvent(t, observer, enums.IMRealtimeEventConversationUpdated)
				requireNoCapturedRealtimeEvent(t, observer)
				requireNoCapturedRealtimeEvent(t, customer)
				requireNoCapturedRealtimeEvent(t, full)
			}
		})
	}
}

func drainEmployeePublication(sessions ...*ClientSession) {
	for _, session := range sessions {
		for len(session.Send) > 0 {
			<-session.Send
		}
	}
}
