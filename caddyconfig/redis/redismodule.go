package redisadapter

import (
	"context"
	"fmt"

	"github.com/caddyserver/caddy/v2"
	"github.com/redis/go-redis/v9"
)

// func init() {
// 	caddy.RegisterModule(RedisConfigLoader{})
// }

type RedisConfigLoader struct {
	Address   string `json:"address,omitempty"`
	Password  string `json:"password,omitempty"`
	DB        int    `json:"db,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	Channel   string `json:"channel,omitempty"`
}

func (RedisConfigLoader) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID: "redisadapter",
		New: func() caddy.Module {
			return new(RedisConfigLoader)
		},
	}
}

func (rcl *RedisConfigLoader) Provision(ctx caddy.Context) error {
	// Set defaults
	if rcl.Address == "" {
		rcl.Address = "localhost:6379"
	}
	if rcl.KeyPrefix == "" {
		rcl.KeyPrefix = "caddy:config"
	}
	if rcl.Channel == "" {
		rcl.Channel = "caddy:config:updates"
	}

	// Create Redis client
	client := redis.NewClient(&redis.Options{
		Addr:     rcl.Address,
		Password: rcl.Password,
		DB:       rcl.DB,
	})

	// Test connection
	if err := client.Ping(context.Background()).Err(); err != nil {
		return fmt.Errorf("failed to connect to redis: %w", err)
	}

	// Load and apply config
	config, err := loadConfigFromRedis(context.Background(), client, rcl.KeyPrefix)
	if err != nil {
		return fmt.Errorf("failed to load config from redis: %w", err)
	}

	// Start pub/sub listener
	go startPubSubListener(client, rcl.KeyPrefix, rcl.Channel)

	// Load the config into Caddy
	return caddy.Load(config, true)
}

var _ caddy.Module = (*RedisConfigLoader)(nil)
var _ caddy.Provisioner = (*RedisConfigLoader)(nil)

// var _ caddy.Validator = (*RedisConfigLoader)(nil)
// var _ caddy.ConfigLoader = (*RedisConfigLoader)(nil)
