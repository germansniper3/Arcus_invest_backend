package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// maxSignatureImageBytes caps a decoded signature PNG. A drawn signature is a
// few tens of KB; anything far larger is a mistake or an attempt to wedge the
// stamping step.
const maxSignatureImageBytes = 2 << 20 // 2 MB

// decodeSignaturePNG accepts either a bare base64 payload or a full
// `data:image/png;base64,...` URL, which is what a canvas produces client-side.
func decodeSignaturePNG(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "a signature image is required")
	}
	if i := strings.Index(raw, ","); strings.HasPrefix(raw, "data:") && i > 0 {
		if !strings.Contains(raw[:i], "image/png") {
			return nil, echo.NewHTTPError(http.StatusBadRequest, "the signature must be a PNG")
		}
		raw = raw[i+1:]
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "the signature image could not be decoded")
	}
	if len(decoded) > maxSignatureImageBytes {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "the signature image is too large")
	}
	return decoded, nil
}

// isPDF reports whether a stored document can actually be stamped. Contracts
// also accept .doc/.docx, which have no page geometry to place a signature on.
func isPDF(contentType, fileName string) bool {
	return strings.Contains(strings.ToLower(contentType), "pdf") ||
		strings.HasSuffix(strings.ToLower(fileName), ".pdf")
}

func contractSignatureJSON(s models.ContractSignature) map[string]any {
	return map[string]any{
		"id":                  s.ID,
		"contract_id":         s.ContractID,
		"signer_id":           s.SignerID,
		"signer_name":         s.SignerName,
		"signer_email":        s.SignerEmail,
		"signer_role":         s.SignerRole,
		"page":                s.Page,
		"position_x":          s.PositionX,
		"position_y":          s.PositionY,
		"width_frac":          s.WidthFrac,
		"signed_at":           s.SignedAt,
		"ip":                  s.IP,
		"user_agent":          s.UserAgent,
		"original_version_id": s.OriginalVersionID,
		"signed_version_id":   s.SignedVersionID,
		"original_hash":       s.OriginalHash,
		"signed_hash":         s.SignedHash,
	}
}

// AdminSignContract stamps a signature onto a contract's current PDF and
// records the signing event.
//
// This is the ONLY route to the `signed` status: AdminUpdateContract refuses to
// set it, so a signed contract always has a ContractSignature behind it. The
// unsigned document is kept as its own version rather than being replaced.
func (h Handler) AdminSignContract(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid contract id"))
	}
	var ct models.Contract
	if err := h.DB.First(&ct, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("contract not found"))
	}

	var req struct {
		Image     string  `json:"image"`
		Page      int     `json:"page"`
		X         float64 `json:"x"`
		Y         float64 `json:"y"`
		WidthFrac float64 `json:"width_frac"`
		Save      bool    `json:"save_signature"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}

	if ct.StoredKey == "" {
		return c.JSON(http.StatusBadRequest, errResponse("attach the contract document before signing it"))
	}
	if !isPDF(ct.ContentType, ct.FileName) {
		return c.JSON(http.StatusBadRequest, errResponse("only PDF contracts can be signed — re-upload this document as a PDF"))
	}
	// Signing is a transition out of `sent`, not a state anyone can jump to.
	if ct.Status == "signed" {
		return c.JSON(http.StatusConflict, errResponse("this contract has already been signed"))
	}
	if ct.Status != "sent" {
		return c.JSON(http.StatusConflict, errResponse("send the contract before signing it — only a sent contract can be signed"))
	}

	// Gated before the signature is decoded or the PDF is touched. Signing is the
	// least reversible action in the system: it stamps a document, writes an
	// evidence record and moves the contract to a state only signing can reach.
	//
	// Approving does not sign. The approver unblocks this contract for this
	// signer, who then signs themselves — the identity, IP and user-agent in
	// ContractSignature have to come from the person actually signing, or the
	// evidence record asserts something that did not happen.
	blocked, gerr := h.gate(c, models.ApprovalContractSign, models.ApprovalEntityContract, ct.ID, ct.Value,
		fmt.Sprintf("Sign contract %q — %s", ct.Title, zmw(ct.Value)))
	if gerr != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
	}
	if blocked != nil {
		return h.blockedResponse(c, blocked)
	}

	signature, err := decodeSignaturePNG(req.Image)
	if err != nil {
		return err
	}
	if req.Page < 1 {
		req.Page = 1
	}

	// Read the current document into memory: stamping needs to seek, and the
	// 15 MB upload cap bounds this.
	rc, err := h.Store.Open(ct.StoredKey)
	if err != nil {
		return c.JSON(http.StatusNotFound, errResponse("the contract file could not be read"))
	}
	originalBytes, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("the contract file could not be read"))
	}

	stamped, err := services.StampSignature(bytes.NewReader(originalBytes), signature, services.SignaturePlacement{
		Page: req.Page, X: req.X, Y: req.Y, WidthFrac: req.WidthFrac,
	})
	if err != nil {
		// These errors name the actual problem (bad page, unreadable PNG), so
		// they are worth showing rather than flattening to "signing failed".
		return c.JSON(http.StatusBadRequest, errResponse(err.Error()))
	}

	// The version the signature was applied to, so the evidence points at a
	// specific document rather than "whatever was current at the time".
	var originalVersion models.DocumentVersion
	var originalVersionID *uuid.UUID
	if err := h.DB.Where("parent_type = ? AND parent_id = ?", models.DocParentContract, ct.ID).
		Order("version DESC").First(&originalVersion).Error; err == nil {
		originalVersionID = &originalVersion.ID
	}

	signedKey := "contracts/" + uuid.NewString() + ".pdf"
	signedHash, err := h.saveHashed(signedKey, bytes.NewReader(stamped), int64(len(stamped)), "application/pdf")
	if err != nil {
		c.Logger().Errorf("signed contract storage failed (key=%s): %v", signedKey, err)
		return c.JSON(http.StatusInternalServerError, errResponse("could not store the signed document"))
	}

	var actor models.User
	h.DB.First(&actor, "id = ?", c.Get("user_id"))

	signedName := strings.TrimSuffix(ct.FileName, ".pdf") + " (signed).pdf"
	signedVersion := models.DocumentVersion{
		ParentType:  models.DocParentContract,
		ParentID:    ct.ID,
		Version:     h.nextDocumentVersion(models.DocParentContract, ct.ID),
		FileName:    signedName,
		StoredKey:   signedKey,
		ContentType: "application/pdf",
		Size:        int64(len(stamped)),
		FileHash:    signedHash,
		Note:        "Signed by " + actor.FullName,
	}
	if actor.ID != uuid.Nil {
		signedVersion.UploadedByID = &actor.ID
		signedVersion.UploadedBy = actor.FullName
	}
	if err := h.DB.Create(&signedVersion).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not record the signed version"))
	}

	// Hash the exact bytes that were signed, rather than trusting the stored
	// hash of the version row, so the record stands on its own.
	originalDigest := sha256.Sum256(originalBytes)
	record := models.ContractSignature{
		ContractID:        ct.ID,
		SignerName:        actor.FullName,
		SignerEmail:       actor.Email,
		SignerRole:        actor.Role,
		Page:              req.Page,
		PositionX:         req.X,
		PositionY:         req.Y,
		WidthFrac:         req.WidthFrac,
		SignedAt:          time.Now().UTC(),
		IP:                c.RealIP(),
		UserAgent:         c.Request().UserAgent(),
		OriginalVersionID: originalVersionID,
		SignedVersionID:   &signedVersion.ID,
		OriginalHash:      hex.EncodeToString(originalDigest[:]),
		SignedHash:        signedHash,
	}
	if actor.ID != uuid.Nil {
		record.SignerID = &actor.ID
	}
	if err := h.DB.Create(&record).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not record the signature"))
	}

	if err := h.DB.Model(&ct).Updates(map[string]any{
		"status": "signed", "file_name": signedName, "stored_key": signedKey,
		"content_type": "application/pdf", "size": int64(len(stamped)),
		"file_hash": signedHash, "current_version": signedVersion.Version,
	}).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update the contract"))
	}

	if req.Save && actor.ID != uuid.Nil {
		saved := models.UserSignature{UserID: actor.ID, ImagePNG: signature}
		// One saved signature per user; re-saving replaces the previous one.
		h.DB.Where("user_id = ?", actor.ID).Delete(&models.UserSignature{})
		if err := h.DB.Create(&saved).Error; err != nil {
			c.Logger().Errorf("could not save the user signature: %v", err)
		}
	}

	h.DB.First(&ct, "id = ?", id)
	return c.JSON(http.StatusOK, map[string]any{
		"contract":  contractJSON(ct),
		"signature": contractSignatureJSON(record),
	})
}

// AdminListContractSignatures returns the signing events recorded for a
// contract, newest first.
func (h Handler) AdminListContractSignatures(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid contract id"))
	}
	var rows []models.ContractSignature
	if err := h.DB.Where("contract_id = ?", id).Order("signed_at DESC").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load signatures"))
	}
	out := []map[string]any{}
	for _, s := range rows {
		out = append(out, contractSignatureJSON(s))
	}
	return c.JSON(http.StatusOK, out)
}

// AdminGetMySignature returns the caller's saved signature PNG, so the signing
// dialog can offer it instead of making them redraw.
func (h Handler) AdminGetMySignature(c echo.Context) error {
	var saved models.UserSignature
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).First(&saved).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("no saved signature"))
	}
	return c.JSON(http.StatusOK, map[string]any{
		"image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(saved.ImagePNG),
	})
}

// AdminDeleteMySignature forgets the caller's saved signature.
func (h Handler) AdminDeleteMySignature(c echo.Context) error {
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).Delete(&models.UserSignature{}).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete the saved signature"))
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "saved signature removed"})
}
