package seed

import (
	"arcusinvest/internal/authz"
	"arcusinvest/internal/models"

	"gorm.io/gorm"
)

// builtInLabels maps role slugs to their human-readable display labels.
var builtInLabels = map[models.Role]string{
	models.RoleSuperAdmin: "Super Admin",
	models.RoleAdmin:      "Admin",
	models.RoleAdmissions: "Admissions",
	models.RoleStudent:    "Student",
}

// builtInDescriptions provides a one-line summary of each built-in role.
var builtInDescriptions = map[models.Role]string{
	models.RoleSuperAdmin: "Full access to all resources including role management. Cannot be restricted.",
	models.RoleAdmin:      "Full access to all business resources. Cannot manage roles.",
	models.RoleAdmissions: "Student intake: enrollments, students, events. Read-only on products and metrics.",
	models.RoleStudent:    "Student portal only. No admin access.",
}

// Roles provisions the four built-in roles and their permissions from
// authz.BuiltInGrants. It is idempotent and safe to run on every boot.
//
// The role row is FirstOrCreate'd (so an admin-edited label/description is
// preserved), but permissions are hard-replaced from the canonical
// BuiltInGrants — the code is authoritative for built-in grants.
//
// All permission replacement uses Unscoped().Delete() to avoid the soft-delete
// + unique-index collision that has bitten this repo before.
func Roles(db *gorm.DB) error {
	for slug, grants := range authz.BuiltInGrants {
		label := builtInLabels[slug]
		if label == "" {
			label = string(slug)
		}
		desc := builtInDescriptions[slug]

		role := models.CustomRole{
			Name:        string(slug),
			Label:       label,
			IsBuiltIn:   true,
			Description: desc,
		}
		if err := db.Where("name = ?", role.Name).FirstOrCreate(&role).Error; err != nil {
			return err
		}

		// Hard-delete existing permissions (Unscoped to bypass soft-delete),
		// then re-create from the canonical BuiltInGrants.
		if err := db.Unscoped().Where("role_id = ?", role.ID).Delete(&models.CustomRolePermission{}).Error; err != nil {
			return err
		}
		for res, grant := range grants {
			perm := models.CustomRolePermission{
				RoleID:    role.ID,
				Resource:  string(res),
				CanRead:   grant.Actions[authz.ActionRead],
				CanCreate: grant.Actions[authz.ActionCreate],
				CanUpdate: grant.Actions[authz.ActionUpdate],
				CanDelete: grant.Actions[authz.ActionDelete],
				Scope:     string(grant.Scope),
			}
			if err := db.Create(&perm).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
