package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"

	"arcusinvest/internal/models"
	"arcusinvest/internal/storage"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// saveHashed streams an upload into storage while computing its SHA256 in the
// same pass. Hashing separately would mean either buffering the whole file in
// memory or reading it back out of storage — and a hash read back from storage
// proves nothing about what the client actually sent.
func (h Handler) saveHashed(key string, r io.Reader, size int64, contentType string) (string, error) {
	digest := sha256.New()
	if err := h.Store.Save(key, io.TeeReader(r, digest), size, contentType); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// nextDocumentVersion returns the version number a new upload should take for a
// parent. Versions are per-parent and 1-based.
func (h Handler) nextDocumentVersion(parentType string, parentID uuid.UUID) int {
	var highest int
	// COALESCE so the first upload on a parent yields 0 → version 1, rather than
	// a NULL scan error.
	h.DB.Model(&models.DocumentVersion{}).
		Where("parent_type = ? AND parent_id = ?", parentType, parentID).
		Select("COALESCE(MAX(version), 0)").
		Scan(&highest)
	return highest + 1
}

// recordDocumentAccess writes a read of a stored file to the access log.
//
// Best-effort and never surfaced to the caller: the download itself must not
// fail because the log write did, matching how audit middleware already treats
// mutations. Logged reads are what make "who downloaded this contract?"
// answerable, which AuditLog cannot answer since it only covers mutations.
func (h Handler) recordDocumentAccess(c echo.Context, parentType string, parentID uuid.UUID, versionID *uuid.UUID, action string) {
	entry := models.DocumentAccessLog{
		ParentType: parentType,
		ParentID:   parentID,
		VersionID:  versionID,
		Action:     action,
		IP:         c.RealIP(),
		UserAgent:  c.Request().UserAgent(),
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		entry.ActorID = &actor.ID
		entry.ActorName = actor.FullName
		entry.ActorRole = actor.Role
	}
	if err := h.DB.Create(&entry).Error; err != nil {
		c.Logger().Errorf("document access log failed (%s/%s): %v", parentType, parentID, err)
	}
}

// BackfillDocumentVersions gives every contract that already has a file a v1
// version row, so history does not begin with a blank page for exactly the
// documents that predate versioning — which are the oldest and most likely to
// matter. Idempotent: contracts that already have versions are skipped.
//
// Hashes are computed by reading the stored bytes back. That is weaker than
// hashing at upload (it proves what is in storage now, not what was uploaded
// then) and is only acceptable because there is no alternative for files
// already at rest. A file that cannot be read still gets its row, with an empty
// hash — knowing a version existed matters more than hashing it.
func BackfillDocumentVersions(db *gorm.DB, store storage.Storage) error {
	var contracts []models.Contract
	if err := db.Where("stored_key <> ''").Find(&contracts).Error; err != nil {
		return err
	}
	for _, ct := range contracts {
		var existing int64
		if err := db.Model(&models.DocumentVersion{}).
			Where("parent_type = ? AND parent_id = ?", models.DocParentContract, ct.ID).
			Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			continue
		}

		hash := ""
		if rc, err := store.Open(ct.StoredKey); err == nil {
			digest := sha256.New()
			if _, err := io.Copy(digest, rc); err == nil {
				hash = hex.EncodeToString(digest.Sum(nil))
			}
			rc.Close()
		}

		version := models.DocumentVersion{
			ParentType:  models.DocParentContract,
			ParentID:    ct.ID,
			Version:     1,
			FileName:    ct.FileName,
			StoredKey:   ct.StoredKey,
			ContentType: ct.ContentType,
			Size:        ct.Size,
			FileHash:    hash,
			Note:        "Recorded when version history was introduced; uploaded earlier.",
		}
		if err := db.Create(&version).Error; err != nil {
			return err
		}
		if err := db.Model(&models.Contract{}).Where("id = ?", ct.ID).
			Updates(map[string]any{"file_hash": hash, "current_version": 1}).Error; err != nil {
			return err
		}
	}
	return nil
}

func documentVersionJSON(v models.DocumentVersion) map[string]any {
	return map[string]any{
		"id":             v.ID,
		"created_at":     v.CreatedAt,
		"version":        v.Version,
		"file_name":      v.FileName,
		"content_type":   v.ContentType,
		"size":           v.Size,
		"file_hash":      v.FileHash,
		"note":           v.Note,
		"uploaded_by_id": v.UploadedByID,
		"uploaded_by":    v.UploadedBy,
	}
}

func documentAccessJSON(a models.DocumentAccessLog) map[string]any {
	return map[string]any{
		"id":         a.ID,
		"created_at": a.CreatedAt,
		"version_id": a.VersionID,
		"actor_id":   a.ActorID,
		"actor_name": a.ActorName,
		"actor_role": a.ActorRole,
		"action":     a.Action,
		"ip":         a.IP,
		"user_agent": a.UserAgent,
	}
}

// AdminListContractVersions returns every stored revision of a contract's
// document, newest first.
func (h Handler) AdminListContractVersions(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid contract id"))
	}
	var ct models.Contract
	if err := h.DB.First(&ct, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("contract not found"))
	}
	var versions []models.DocumentVersion
	if err := h.DB.Where("parent_type = ? AND parent_id = ?", models.DocParentContract, id).
		Order("version DESC").Find(&versions).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load versions"))
	}
	// Must be an empty slice, never nil: a nil slice marshals to JSON `null` and
	// clients doing `versions.length` would crash on a contract with no file.
	out := []map[string]any{}
	for _, v := range versions {
		out = append(out, documentVersionJSON(v))
	}
	return c.JSON(http.StatusOK, out)
}

// AdminDownloadContractVersion streams one specific revision of a contract
// document, so a superseded file stays reachable after a replacement upload.
func (h Handler) AdminDownloadContractVersion(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid contract id"))
	}
	versionID, err := uuid.Parse(c.Param("versionId"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid version id"))
	}
	var v models.DocumentVersion
	// Scoped by parent as well as id so a version id cannot be used to read a
	// document hanging off a different contract.
	if err := h.DB.First(&v, "id = ? AND parent_type = ? AND parent_id = ?",
		versionID, models.DocParentContract, id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("version not found"))
	}
	reader, err := h.Store.Open(v.StoredKey)
	if err != nil {
		return c.JSON(http.StatusNotFound, errResponse("version file not found"))
	}
	defer reader.Close()

	h.recordDocumentAccess(c, models.DocParentContract, id, &v.ID, "download")

	contentType := v.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	filename := v.FileName
	if filename == "" {
		filename = "contract"
	}
	c.Response().Header().Set(echo.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%q", filename))
	return c.Stream(http.StatusOK, contentType, reader)
}

// AdminListContractAccessLog answers "who has read this contract?".
func (h Handler) AdminListContractAccessLog(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid contract id"))
	}
	var entries []models.DocumentAccessLog
	if err := h.DB.Where("parent_type = ? AND parent_id = ?", models.DocParentContract, id).
		Order("created_at DESC").Limit(200).Find(&entries).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the access log"))
	}
	out := []map[string]any{}
	for _, a := range entries {
		out = append(out, documentAccessJSON(a))
	}
	return c.JSON(http.StatusOK, out)
}
