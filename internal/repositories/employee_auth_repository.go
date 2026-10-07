package repositories

import (
	"time"

	"agent-desk/internal/models"
	"agent-desk/internal/pkg/enums"

	"gorm.io/gorm"
)

type EmployeeAuthOverride struct {
	Code      string
	Effect    int
	ExpiredAt *time.Time
}
type employeeAuthRepository struct{}

var EmployeeAuthRepository = newEmployeeAuthRepository()

func newEmployeeAuthRepository() *employeeAuthRepository { return &employeeAuthRepository{} }

func (r *employeeAuthRepository) LoginSessionByID(db *gorm.DB, id int64) (*models.LoginSession, error) {
	var row models.LoginSession
	if err := db.First(&row, id).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *employeeAuthRepository) UserByID(db *gorm.DB, id int64) (*models.User, error) {
	var row models.User
	if err := db.First(&row, id).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *employeeAuthRepository) UserRoles(tx *gorm.DB, userID int64) ([]models.Role, error) {
	roles := make([]models.Role, 0)
	if err := tx.
		Table("t_role AS r").
		Select("r.*").
		Joins("JOIN t_user_role AS ur ON ur.role_id = r.id").
		Where("ur.user_id = ? AND r.status = ?", userID, enums.StatusOk).
		Order("r.sort_no ASC, r.id ASC").
		Scan(&roles).Error; err != nil {
		return nil, err
	}

	return roles, nil
}

func (r *employeeAuthRepository) RolePermissionCodesQuery(tx *gorm.DB, userID int64) *gorm.DB {
	return tx.Table("t_permission AS p").
		Select("DISTINCT p.code").
		Joins("JOIN t_role_permission AS rp ON rp.permission_id = p.id").
		Joins("JOIN t_user_role AS ur ON ur.role_id = rp.role_id").
		Joins("JOIN t_role AS r ON r.id = rp.role_id").
		Where("ur.user_id = ?", userID).
		Where("p.status = ?", enums.StatusOk).
		Where("r.status = ?", enums.StatusOk)
}

func (r *employeeAuthRepository) ActiveOverridesQuery(tx *gorm.DB, userID int64, now time.Time) *gorm.DB {
	return tx.Table("t_user_permission AS up").
		Select("p.code, up.effect, up.expired_at").
		Joins("JOIN t_permission AS p ON p.id = up.permission_id").
		Where("up.user_id = ? AND (up.expired_at IS NULL OR up.expired_at > ?)", userID, now).
		Where("p.status = ?", enums.StatusOk)
}
