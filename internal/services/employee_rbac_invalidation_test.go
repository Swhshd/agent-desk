package services

import (
	"errors"
	"reflect"
	"testing"

	"agent-desk/internal/models"
	"agent-desk/internal/pkg/dto"
	"agent-desk/internal/pkg/enums"
	"gorm.io/gorm"
)

func employeeStatusMutation(s *userService, mode string, id int64, actor *dto.AuthPrincipal) error {
	if mode == "delete" {
		return s.DeleteUser(id, actor)
	}
	status := enums.StatusDisabled
	if mode == "deleted status" {
		status = enums.StatusDeleted
	}
	return s.UpdateStatus(id, int(status), actor)
}

func TestEmployeeStatusInvalidationBeforeRevokeFailure(t *testing.T) {
	for _, mode := range []string{"disable", "deleted status", "delete"} {
		t.Run(mode, func(t *testing.T) {
			db, l, user, actor := employeeMutationFixture(t)
			row := employeeMutationLogin(t, db, user.ID, "status")
			a, b := employeeMutationSockets(t, l, user.ID, row.ID, "target")
			otherA, otherB := employeeMutationSockets(t, l, user.ID+1, row.ID+1, "other")
			failure := errors.New("injected later session update failure")
			employeeMutationFailUpdate(t, db, "t_login_session", failure)
			calls := 0
			wantStatus := enums.StatusDisabled
			if mode == "deleted status" {
				wantStatus = enums.StatusDeleted
			}
			l.onEmployees = func(ids []int64) {
				calls++
				persisted := newUserService(l).Get(user.ID)
				if !reflect.DeepEqual(ids, []int64{user.ID}) || persisted.Status != wantStatus || (mode == "delete" && persisted.DeletedAt == nil) || employeeMutationReadLogin(t, db, row.ID).RevokedAt != nil {
					t.Fatal("status close ran before status persistence or after revoke")
				}
			}
			err := employeeStatusMutation(newUserService(l), mode, user.ID, actor)
			persisted := newUserService(l).Get(user.ID)
			if !errors.Is(err, failure) || calls != 1 || persisted.Status != wantStatus || (mode == "delete" && persisted.DeletedAt == nil) || !a.Closed.Load() || !b.Closed.Load() || otherA.Closed.Load() || otherB.Closed.Load() {
				t.Fatal("persisted status did not close target before later revoke failure")
			}
		})
	}
}

func TestEmployeeStatusWriteFailureDoesNotInvalidate(t *testing.T) {
	for _, mode := range []string{"disable", "deleted status", "delete"} {
		t.Run(mode, func(t *testing.T) {
			db, l, user, actor := employeeMutationFixture(t)
			row := employeeMutationLogin(t, db, user.ID, "status")
			a, b := employeeMutationSockets(t, l, user.ID, row.ID, "target")
			failure := errors.New("injected user update failure")
			employeeMutationFailUpdate(t, db, "t_user", failure)
			l.onEmployees = func([]int64) { t.Fatal("failed status write invalidated") }
			err := employeeStatusMutation(newUserService(l), mode, user.ID, actor)
			persisted := newUserService(l).Get(user.ID)
			if !errors.Is(err, failure) || persisted.Status != enums.StatusOk || persisted.DeletedAt != nil || a.Closed.Load() || b.Closed.Load() || employeeMutationReadLogin(t, db, row.ID).RevokedAt != nil {
				t.Fatal("failed first write changed status/session/socket state")
			}
		})
	}
}

func TestEmployeeStatusSuccess(t *testing.T) {
	for _, mode := range []string{"disable", "deleted status", "delete", "enable"} {
		t.Run(mode, func(t *testing.T) {
			db, l, user, actor := employeeMutationFixture(t)
			row := employeeMutationLogin(t, db, user.ID, "status")
			a, b := employeeMutationSockets(t, l, user.ID, row.ID, "target")
			svc := newUserService(l)
			var err error
			if mode == "enable" {
				err = svc.UpdateStatus(user.ID, int(enums.StatusOk), actor)
			} else {
				err = employeeStatusMutation(svc, mode, user.ID, actor)
			}
			if err != nil {
				t.Fatal(err)
			}
			wantClosed := mode != "enable"
			if a.Closed.Load() != wantClosed || b.Closed.Load() != wantClosed || (employeeMutationReadLogin(t, db, row.ID).RevokedAt != nil) != wantClosed {
				t.Fatal("status success changed wrong lifecycle state")
			}
		})
	}
}

func employeeMutationRoles(t *testing.T, db *gorm.DB, userID int64) (models.Role, models.Role) {
	t.Helper()
	oldRole := models.Role{Code: "old", Name: "old", Status: enums.StatusOk}
	newRole := models.Role{Code: "new", Name: "new", Status: enums.StatusOk}
	for _, row := range []any{&oldRole, &newRole} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&models.UserRole{UserID: userID, RoleID: oldRole.ID}).Error; err != nil {
		t.Fatal(err)
	}
	return oldRole, newRole
}

func employeeMutationRoleIDs(t *testing.T, db *gorm.DB, userID int64) []int64 {
	t.Helper()
	var ids []int64
	if err := db.Model(&models.UserRole{}).Where("user_id = ?", userID).Order("role_id").Pluck("role_id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestEmployeeAssignRolesCommitOrdering(t *testing.T) {
	for _, failRevoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "later revoke failure"}[failRevoke], func(t *testing.T) {
			db, l, user, actor := employeeMutationFixture(t)
			_, newRole := employeeMutationRoles(t, db, user.ID)
			row := employeeMutationLogin(t, db, user.ID, "roles")
			a, b := employeeMutationSockets(t, l, user.ID, row.ID, "target")
			otherA, otherB := employeeMutationSockets(t, l, user.ID+1, row.ID+1, "other")
			failure := errors.New("injected later revoke failure")
			if failRevoke {
				employeeMutationFailUpdate(t, db, "t_login_session", failure)
			}
			calls := 0
			l.onEmployees = func(ids []int64) {
				calls++
				if !reflect.DeepEqual(ids, []int64{user.ID}) || !reflect.DeepEqual(employeeMutationRoleIDs(t, db, user.ID), []int64{newRole.ID}) {
					t.Fatal("role invalidation did not see committed target membership")
				}
				if calls == 1 && employeeMutationReadLogin(t, db, row.ID).RevokedAt != nil {
					t.Fatal("role invalidation ran after revoke-all")
				}
			}
			err := newUserService(l).AssignRoles(user.ID, []int64{newRole.ID}, actor)
			wantCalls := 2
			if failRevoke {
				wantCalls = 1
				if !errors.Is(err, failure) {
					t.Fatalf("later revoke error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if calls != wantCalls || !a.Closed.Load() || !b.Closed.Load() || otherA.Closed.Load() || otherB.Closed.Load() {
				t.Fatal("committed role membership did not invalidate before revoke-all")
			}
		})
	}
}

func TestEmployeeAssignRolesRollbackNoInvalidation(t *testing.T) {
	for _, mode := range []string{"invalid role", "insert failure", "commit failure"} {
		t.Run(mode, func(t *testing.T) {
			db, l, user, actor := employeeMutationFixture(t)
			oldRole, newRole := employeeMutationRoles(t, db, user.ID)
			row := employeeMutationLogin(t, db, user.ID, "roles")
			a, b := employeeMutationSockets(t, l, user.ID, row.ID, "target")
			roleIDs := []int64{newRole.ID, 999999}
			if mode == "insert failure" {
				roleIDs = []int64{newRole.ID}
				const name = "test:role_insert_failure"
				if err := db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
					if tx.Statement.Table == "t_user_role" {
						tx.AddError(errors.New("injected membership insert failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Callback().Create().Remove(name) })
			}
			if mode == "commit failure" {
				roleIDs = []int64{newRole.ID}
				const name = "test:role_commit_failure"
				if err := db.Callback().Create().After("gorm:create").Register(name, func(tx *gorm.DB) {
					if tx.Statement.Table == "t_user_role" {
						if err := tx.Rollback().Error; err != nil {
							t.Fatal(err)
						}
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Callback().Create().Remove(name) })
			}
			l.onEmployees = func([]int64) { t.Fatal("rolled-back role mutation invalidated") }
			if err := newUserService(l).AssignRoles(user.ID, roleIDs, actor); err == nil {
				t.Fatal("expected membership transaction failure")
			}
			if !reflect.DeepEqual(employeeMutationRoleIDs(t, db, user.ID), []int64{oldRole.ID}) || a.Closed.Load() || b.Closed.Load() || employeeMutationReadLogin(t, db, row.ID).RevokedAt != nil {
				t.Fatal("rollback changed membership/session/socket state")
			}
		})
	}
}

func TestEmployeeAssignRolesTransactionRoleReads(t *testing.T) {
	db, l, user, actor := employeeMutationFixture(t)
	_, newRole := employeeMutationRoles(t, db, user.ID)
	const name = "test:require_transaction_role_read"
	reads := 0
	if err := db.Callback().Query().Before("gorm:query").Register(name, func(query *gorm.DB) {
		if query.Statement.Table == "t_role" {
			reads++
			if _, ok := query.Statement.ConnPool.(gorm.TxCommitter); !ok {
				query.AddError(errors.New("role lookup escaped membership transaction"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(name) })
	if err := newUserService(l).AssignRoles(user.ID, []int64{newRole.ID}, actor); err != nil {
		t.Fatalf("transactional role assignment failed: %v", err)
	}
	if reads != 1 || !reflect.DeepEqual(employeeMutationRoleIDs(t, db, user.ID), []int64{newRole.ID}) {
		t.Fatal("role lookup did not validate new committed membership")
	}
}
