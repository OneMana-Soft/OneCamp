package redisInit

import (
	"context"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/redis/go-redis/v9"
)

var RedisClient *redis.Client

type RedisConnConfig struct {
	Password string
	DB       int
	Host     string
}

func ConnectRedis(config *RedisConnConfig) (err error) {
	ctx := context.Background()
	RedisClient = redis.NewClient(&redis.Options{
		Addr:     config.Host,
		Password: config.Password, // no password set
		DB:       config.DB,       // use default DB
	})

	err = RedisClient.Ping(ctx).Err()

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf("Failed to connect to redis err:= %+v", err)
		return err
	}

	helpers.MessageLogs.InfoLog.Println("Successfully connected to redis !")

	return
}
