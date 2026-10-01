package minioInit

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MinoConfig struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
}

var MinioClient *minio.Client

func ConnectMinio(ctx context.Context, minoConfig *MinoConfig) (err error) {
	endpoint := minoConfig.Endpoint
	accessKeyID := minoConfig.AccessKeyID
	secretAccessKey := minoConfig.SecretAccessKey
	useSSL := minoConfig.UseSSL

	// Initialize minioInit client object.
	minioClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "minioInit/ConnectMinio Failed to create minio client")
		return
	}

	MinioClient = minioClient

	helpers.MessageLogs.InfoLog.Println("Successfully connected with minio server !")

	return
}
