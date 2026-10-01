package controllers

import (
	"net/http"
	"path/filepath"
	"strings"

	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

type EmailConfigRequest struct {
	SenderEmail string `json:"sender_email"`
	Subject     string `json:"invitation_email_subject"`
	Template    string `json:"invitation_email_template"`
}

// GetEmailConfig handles GET /admin/config/email
func GetEmailConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	configs, err := configModels.GetMultipleConfigsByKeys([]string{
		"sender_email",
		"invitation_email_subject",
		"invitation_email_template",
		"invitation_email_logo",
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetEmailConfig Failed to get email configs err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to get email configuration",
			"status": "failed",
		})
		return
	}

	result := make(map[string]interface{})
	for _, cfg := range configs {
		if cfg.Key == "invitation_email_logo" {
			result["has_logo"] = cfg.Value != ""
		} else {
			result[cfg.Key] = cfg.Value
		}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data":   result,
	})
}

// UpdateEmailConfig handles POST /admin/config/email
func UpdateEmailConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var requestBody EmailConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	if requestBody.SenderEmail != "" {
		if err := configModels.UpsertConfig("sender_email", requestBody.SenderEmail); err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/UpdateEmailConfig Failed to update sender_email err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
				"msg":    "failed to update sender email",
				"status": "failed",
			})
			return
		}
	}

	if requestBody.Subject != "" {
		if err := configModels.UpsertConfig("invitation_email_subject", requestBody.Subject); err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/UpdateEmailConfig Failed to update subject err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
				"msg":    "failed to update email subject",
				"status": "failed",
			})
			return
		}
	}

	if requestBody.Template != "" {
		if err := configModels.UpsertConfig("invitation_email_template", requestBody.Template); err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/UpdateEmailConfig Failed to update template err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
				"msg":    "failed to update email template",
				"status": "failed",
			})
			return
		}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "email configuration updated successfully",
		"status": "success",
	})
}

// UploadEmailLogo handles POST /admin/config/email/logo
func UploadEmailLogo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	MultipartUploadLimit := int64(2 * 1024 * 1024) // 2 MB max file size
	err := r.ParseMultipartForm(MultipartUploadLimit)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UploadEmailLogo Failed to ParseMultipartForm err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "File too large or invalid multipart form",
		})
		return
	}

	file, fHeader, err := r.FormFile("logo")
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UploadEmailLogo Failed to get logo file err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get logo file",
		})
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(fHeader.Filename))
	var contentType string
	if ext == ".png" {
		contentType = "image/png"
	} else if ext == ".jpeg" || ext == ".jpg" {
		contentType = "image/jpeg"
	} else if ext == ".svg" {
		contentType = "image/svg+xml"
	} else if ext == ".webp" {
		contentType = "image/webp"
	} else {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Only PNG, JPEG, SVG, and WebP images are supported",
		})
		return
	}

	bucketName := helpers.UserUploadBucket()

	u := uuid.New()
	fullObjName := "systemFileUpload/email_logo_" + u.String() + ext

	_, err = minioInit.MinioClient.PutObject(ctx, bucketName, fullObjName, file, fHeader.Size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UploadEmailLogo Failed to upload the file err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Failed to upload the logo",
		})
		return
	}

	if err := configModels.UpsertConfig("invitation_email_logo", fullObjName); err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UploadEmailLogo Failed to update config err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Failed to update logo config",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Logo uploaded successfully",
		"data": map[string]interface{}{
			"has_logo": true,
		},
	})
}

// DeleteEmailLogo handles DELETE /admin/config/email/logo
func DeleteEmailLogo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	// Fetch existing to see if we should delete from minio (optional cleanup, but good practice)
	cfg, err := configModels.GetConfigByKey("invitation_email_logo")
	if err == nil && cfg != nil && cfg.Value != "" {
		bucketName := helpers.UserUploadBucket()
		_ = minioInit.MinioClient.RemoveObject(ctx, bucketName, cfg.Value, minio.RemoveObjectOptions{})
	}

	// Update the config string to be empty
	if err := configModels.UpsertConfig("invitation_email_logo", ""); err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteEmailLogo Failed to update config err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Failed to delete logo",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Logo deleted successfully",
		"data": map[string]interface{}{
			"has_logo": false,
		},
	})
}
