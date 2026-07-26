package models

import "github.com/google/uuid"

// CustomRole is a named set of permissions, either built-in (seeded from
// authz.BuiltInGrants on every boot) or created by an admin through the
// role-management UI. The Name slug is stored in User.Role.
type CustomRole struct {
	BaseModel
	Name        string                 `json:"name" gorm:"uniqueIndex;not null"`
	Label       string                 `json:"label" gorm:"not null"`
	IsBuiltIn   bool                   `json:"is_built_in" gorm:"default:false;index"`
	Description string                 `json:"description" gorm:"type:text"`
	Permissions []CustomRolePermission `json:"permissions" gorm:"foreignKey:RoleID;constraint:OnDelete:CASCADE"`
}

// CustomRolePermission grants a CustomRole access to one resource.
// Actions are individual bools for queryability and direct mapping to the
// frontend's { read, create, update, delete } shape.
//
// The composite unique index idx_role_resource on (role_id, resource) prevents
// duplicate grants. Because BaseModel includes gorm.DeletedAt (soft delete),
// all permission replacement MUST use Unscoped().Delete() to avoid the
// soft-deleted row occupying the unique index and causing duplicate-key errors.
type CustomRolePermission struct {
	BaseModel
	RoleID    uuid.UUID `json:"role_id" gorm:"type:uuid;uniqueIndex:idx_role_resource;not null"`
	Resource  string    `json:"resource" gorm:"uniqueIndex:idx_role_resource;not null"`
	CanRead   bool      `json:"can_read" gorm:"default:false"`
	CanCreate bool      `json:"can_create" gorm:"default:false"`
	CanUpdate bool      `json:"can_update" gorm:"default:false"`
	CanDelete bool      `json:"can_delete" gorm:"default:false"`
	Scope     string    `json:"scope" gorm:"type:varchar(10);default:'none'"`
}
