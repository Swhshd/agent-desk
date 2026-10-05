package services

import "agent-desk/internal/pkg/enums"

type employeeConversationQueuePayload struct {
	ConversationID      int64                           `json:"conversationId,omitempty"`
	Status              enums.IMConversationStatus      `json:"status,omitempty"`
	ServiceMode         enums.IMConversationServiceMode `json:"serviceMode,omitempty"`
	CurrentAssigneeID   int64                           `json:"currentAssigneeId,omitempty"`
	CurrentTeamID       int64                           `json:"currentTeamId,omitempty"`
	LastMessageID       int64                           `json:"lastMessageId,omitempty"`
	LastMessageAt       string                          `json:"lastMessageAt,omitempty"`
	LastActiveAt        string                          `json:"lastActiveAt,omitempty"`
	CustomerUnreadCount int                             `json:"customerUnreadCount,omitempty"`
	AgentUnreadCount    int                             `json:"agentUnreadCount,omitempty"`
}

func (employeeConversationQueuePayload) realtimeEventPayload() {}

type employeeMessageQueuePayload struct {
	ConversationID    int64                      `json:"conversationId,omitempty"`
	MessageID         int64                      `json:"messageId,omitempty"`
	SenderType        enums.IMSenderType         `json:"senderType,omitempty"`
	Status            enums.IMConversationStatus `json:"status,omitempty"`
	CurrentAssigneeID int64                      `json:"currentAssigneeId,omitempty"`
}

func (employeeMessageQueuePayload) realtimeEventPayload() {}

type employeeMessageRecallQueuePayload struct {
	ConversationID int64 `json:"conversationId,omitempty"`
	MessageID      int64 `json:"messageId,omitempty"`
}

func (employeeMessageRecallQueuePayload) realtimeEventPayload() {}

// Only the actual registered delivery destination selects authorization and
// audience. Concrete payload validation also applies to full destinations.
func (s *wsService) employeeEventForDelivery(session *ClientSession, deliveryTopic string, event RealtimeEvent) (RealtimeEvent, bool) {
	class := classifyEmployeeRealtimeEvent(event.Type)
	if !s.CanReceiveEvent(session, deliveryTopic, class) {
		return RealtimeEvent{}, false
	}
	audience := s.employeeDeliveryAudience(session, deliveryTopic, class)
	var queuePayload RealtimeEventPayload
	switch class {
	case employeeEventConversationChanged:
		payload, ok := event.Data.(RealtimeConversationChangedPayload)
		if !ok {
			return RealtimeEvent{}, false
		}
		queuePayload = employeeConversationQueuePayload{
			ConversationID:      payload.ConversationID,
			Status:              payload.Status,
			ServiceMode:         payload.ServiceMode,
			CurrentAssigneeID:   payload.CurrentAssigneeID,
			CurrentTeamID:       payload.CurrentTeamID,
			LastMessageID:       payload.LastMessageID,
			LastMessageAt:       payload.LastMessageAt,
			LastActiveAt:        payload.LastActiveAt,
			CustomerUnreadCount: payload.CustomerUnreadCount,
			AgentUnreadCount:    payload.AgentUnreadCount,
		}
	case employeeEventMessageCreated:
		payload, ok := event.Data.(RealtimeMessageCreatedPayload)
		if !ok {
			return RealtimeEvent{}, false
		}
		queuePayload = employeeMessageQueuePayload{
			ConversationID:    payload.ConversationID,
			MessageID:         payload.MessageID,
			SenderType:        payload.SenderType,
			Status:            payload.Status,
			CurrentAssigneeID: payload.CurrentAssigneeID,
		}
	case employeeEventMessageRecalled:
		payload, ok := event.Data.(RealtimeMessageRecalledPayload)
		if !ok {
			return RealtimeEvent{}, false
		}
		queuePayload = employeeMessageRecallQueuePayload{ConversationID: payload.ConversationID, MessageID: payload.MessageID}
	case employeeEventResync:
		payload, ok := event.Data.(RealtimeResyncRequiredPayload)
		if !ok {
			return RealtimeEvent{}, false
		}
		queuePayload = payload
	case employeeEventControl:
		switch event.Type {
		case enums.IMRealtimeEventConnected:
			if _, ok := event.Data.(RealtimeConnectedPayload); !ok {
				return RealtimeEvent{}, false
			}
		case enums.IMRealtimeEventSubscribed, enums.IMRealtimeEventUnsubscribed:
			if _, ok := event.Data.(RealtimeTopicsPayload); !ok {
				return RealtimeEvent{}, false
			}
		case enums.IMRealtimeEventPong:
			if event.Data != nil {
				return RealtimeEvent{}, false
			}
		default:
			return RealtimeEvent{}, false
		}
		queuePayload = event.Data
	default:
		return RealtimeEvent{}, false
	}
	if audience == employeeAudienceQueue {
		event.Data = queuePayload
	}
	return event, true
}
