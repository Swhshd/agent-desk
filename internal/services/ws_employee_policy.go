package services

import (
	"slices"

	"agent-desk/internal/pkg/constants"
	"agent-desk/internal/pkg/enums"
)

type employeeRealtimeEventClass uint8

const (
	employeeEventUnknown employeeRealtimeEventClass = iota
	employeeEventControl
	employeeEventConversationChanged
	employeeEventMessageCreated
	employeeEventMessageRecalled
	employeeEventResync
)

type employeeRealtimeAudience uint8

const (
	employeeAudienceDenied employeeRealtimeAudience = iota
	employeeAudienceQueue
	employeeAudienceFull
)

func classifyEmployeeRealtimeEvent(eventType string) employeeRealtimeEventClass {
	switch eventType {
	case enums.IMRealtimeEventConnected, enums.IMRealtimeEventPong,
		enums.IMRealtimeEventSubscribed, enums.IMRealtimeEventUnsubscribed:
		return employeeEventControl
	case enums.IMRealtimeEventConversationCreated, enums.IMRealtimeEventConversationUpdated,
		enums.IMRealtimeEventConversationAssigned, enums.IMRealtimeEventConversationTransferred,
		enums.IMRealtimeEventConversationClosed, enums.IMRealtimeEventConversationRead:
		return employeeEventConversationChanged
	case enums.IMRealtimeEventMessageCreated:
		return employeeEventMessageCreated
	case enums.IMRealtimeEventMessageRecalled:
		return employeeEventMessageRecalled
	case enums.IMRealtimeEventResyncRequired:
		return employeeEventResync
	default:
		return employeeEventUnknown
	}
}

func employeeHasConversationView(session *ClientSession) bool {
	return session != nil && session.Role == realtimeRoleAdmin &&
		session.Principal != nil && session.Principal.UserID > 0 &&
		slices.Contains(session.Principal.Permissions, constants.PermissionConversationView.Code)
}

// CanSubscribeTopic admits an employee registration destination. Customer and
// notification registrations retain their existing service branches.
func (s *wsService) CanSubscribeTopic(session *ClientSession, candidateTopic string) bool {
	if session == nil || session.Role != realtimeRoleAdmin || session.Principal == nil || session.Principal.UserID <= 0 {
		return false
	}
	if candidateTopic == s.adminTopic(session.Principal.UserID) {
		return true
	}
	if !employeeHasConversationView(session) {
		return false
	}
	if candidateTopic == realtimeTopicAdminAll {
		return true
	}
	_, ok := parseConversationTopic(candidateTopic)
	return ok
}

// CanReceiveEvent takes the actual registered destination selected by fan-out.
// Registry membership is supplied by the manager's locked delivery lookup; the
// event's serialized topic and the session's Topics map are not policy inputs.
func (s *wsService) CanReceiveEvent(session *ClientSession, deliveryTopic string, eventClass employeeRealtimeEventClass) bool {
	if !s.CanSubscribeTopic(session, deliveryTopic) {
		return false
	}
	switch eventClass {
	case employeeEventControl:
		return true
	case employeeEventConversationChanged, employeeEventMessageCreated, employeeEventMessageRecalled, employeeEventResync:
		return employeeHasConversationView(session)
	default:
		return false
	}
}

func (s *wsService) employeeDeliveryAudience(session *ClientSession, deliveryTopic string, eventClass employeeRealtimeEventClass) employeeRealtimeAudience {
	if !s.CanReceiveEvent(session, deliveryTopic, eventClass) {
		return employeeAudienceDenied
	}
	if _, ok := parseConversationTopic(deliveryTopic); ok {
		return employeeAudienceFull
	}
	return employeeAudienceQueue
}
