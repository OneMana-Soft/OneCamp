package redisInit

import (
	"context"
	"strings"

	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
)

func InitRedisExpiryWorker() {
	ctx := context.Background()
	expiryPubSub := RedisClient.PSubscribe(ctx, "__keyevent@0__:expired")

	go func() {
		for msg := range expiryPubSub.Channel() {
			key := msg.Payload
			if strings.HasPrefix(key, "refresh") {
				handleExpiredFCMToken(ctx, key)
			}
		}
	}()
}

func handleExpiredFCMToken(ctx context.Context, key string) {
	strArray := strings.Split(",", key)
	userId := strArray[1]
	deviceId := strArray[2]

	err := userFCMtokenBusiness.DeleteByUserIdAndDeviceId(ctx, userId, deviceId)
	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf("Failed to delete FCM token err:= %+v", err)
	}

}
