package main

import (
	"context"
	"log"
	"os"

	"github.com/redis/go-redis/v9"
)

// RDB is the global Redis client. Nil when Redis is not configured.
var RDB *redis.Client

const (
	redisChanAll     = "cas:broadcast"
	redisChanSession = "cas:session:"
)

// ConnectRedis connects to Redis using REDIS_URL from the environment.
func ConnectRedis(ctx context.Context) error {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		log.Printf("REDIS_URL not set — WebSocket broadcasts are local only (single-pod mode)")
		return nil
	}

	opts, err := redis.ParseURL(url)
	if err != nil {
		return err
	}

	rdb := redis.NewClient(opts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		return err
	}

	RDB = rdb
	log.Printf("connected to Redis at %s", opts.Addr)
	return nil
}
