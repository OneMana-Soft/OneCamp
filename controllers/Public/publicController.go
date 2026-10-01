package controllers

import (
	"net/http"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
)

// GetEmailLogo handles GET /public/email/logo
func GetEmailLogo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. Get the logo object key from system_configs
	cfg, err := configModels.GetConfigByKey("invitation_email_logo")
	if err != nil || cfg == nil || cfg.Value == "" {
		// No logo is set
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"msg": "Logo not found",
		})
		return
	}

	objectKey := cfg.Value

	// 2. Generate a short-lived presigned URL
	bucketName := helpers.UserUploadBucket()

	url, err := minioInit.MinioClient.PresignedGetObject(ctx, bucketName, objectKey, 1*time.Minute, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetEmailLogo Failed to generate presigned URL err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Failed to get logo",
		})
		return
	}

	// 3. Issue a 302 Redirect to the MinIO URL
	// Note: We use 302 (Found) since the presigned URL expires and changes every request.
	http.Redirect(w, r, url.String(), http.StatusFound)
}
