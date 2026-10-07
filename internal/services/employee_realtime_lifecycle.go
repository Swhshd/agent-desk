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
