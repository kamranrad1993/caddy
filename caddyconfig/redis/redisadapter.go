package redisadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

func init() {
	caddyconfig.RegisterAdapter("redis", RedisAdapter{})
}

// RedisAdapter implements the Caddy config adapter interface
type RedisAdapter struct {
}

// Adapt loads the Caddy configuration from Redis
func (a RedisAdapter) Adapt(body []byte, options map[string]interface{}) ([]byte, []caddyconfig.Warning, error) {
	ctx := context.Background()

	// Parse Redis connection info from body or environment
	var redisConfig RedisConfig

	// If body is empty, try environment variables
	if len(body) == 0 || string(body) == "{}" {
		redisConfig = getConfigFromEnv()
	} else {
		if err := json.Unmarshal(body, &redisConfig); err != nil {
			return nil, nil, fmt.Errorf("failed to parse redis config: %w", err)
		}
	}

	// Set defaults
	if redisConfig.Addr == "" {
		redisConfig.Addr = "localhost:6379"
	}
	if redisConfig.KeyPrefix == "" {
		redisConfig.KeyPrefix = "caddy:config"
	}
	if redisConfig.Channel == "" {
		redisConfig.Channel = "caddy:config:updates"
	}

	// Create Redis client
	client := redis.NewClient(&redis.Options{
		Addr:     redisConfig.Addr,
		Password: redisConfig.Password,
		DB:       redisConfig.DB,
	})

	// Test connection
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, nil, fmt.Errorf("failed to connect to redis: %w", err)
	}

	// Load initial config from Redis
	config, err := loadConfigFromRedis(ctx, client, redisConfig.KeyPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load config from redis: %w", err)
	}

	// Set notify-keyspace-events = KEA
	err = client.ConfigSet(ctx, "notify-keyspace-events", "KEA").Err()
	if err != nil {
		log.Fatalf("Failed to set Redis config: %v", err)
	}

	// Start pub/sub listener in background
	go startPubSubListener(client, redisConfig.KeyPrefix, &config)

	return []byte(config), nil, nil
}

type RedisConfig struct {
	Addr      string `json:"addr"`
	Password  string `json:"password,omitempty"`
	DB        int    `json:"db"`
	KeyPrefix string `json:"key_prefix"`
	Channel   string `json:"channel"`
}

func getConfigFromEnv() RedisConfig {
	cfg := RedisConfig{
		Addr:      os.Getenv("REDIS_ADDR"),
		Password:  os.Getenv("REDIS_PASSWORD"),
		KeyPrefix: os.Getenv("REDIS_KEY_PREFIX"),
		Channel:   os.Getenv("REDIS_CHANNEL"),
	}

	// Parse DB as int
	if dbStr := os.Getenv("REDIS_DB"); dbStr != "" {
		fmt.Sscanf(dbStr, "%d", &cfg.DB)
	}

	return cfg
}

func loadConfigFromRedis(ctx context.Context, client *redis.Client, keyPrefix string) (string, error) {
	pattern := keyPrefix + ":*"
	var cursor uint64

	config := ""

	for {
		keys, nextCursor, err := client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return "", fmt.Errorf("failed to scan keys: %w", err)
		}

		for _, key := range keys {
			val, err := client.Get(ctx, key).Result()
			if err != nil {
				continue // Skip missing keys
			}
			jsonPath := strings.TrimPrefix(key, keyPrefix+":")

			config, err = sjson.Set(config, jsonPath, val)
			if err != nil {
				return "", fmt.Errorf("failed to set value for path %s: %w", jsonPath, err)
			}
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return config, nil
}

func startPubSubListener(client *redis.Client, keyPrefix string, currentConfig *string) {
	ctx := context.Background()
	keyspacePrefix := "__keyspace@0__:"
	pubsub := client.PSubscribe(ctx, keyspacePrefix+keyPrefix+":*")
	defer pubsub.Close()

	logger := caddy.Log().Named("redis-config-loader")
	prefix := "__keyspace@0__:" + keyPrefix + ":"

	ch := pubsub.Channel()
	for msg := range ch {
		logger.Info("received config update", zap.String("message", msg.Channel))

		// // Small delay to allow multiple updates to complete
		// time.Sleep(100 * time.Millisecond)
		key := strings.TrimPrefix(msg.Channel, keyspacePrefix)
		jsonPath := strings.TrimPrefix(msg.Channel, prefix)

		value := client.Get(ctx, key)

		switch msg.Payload {
		case "set", "rename_to":
			var err error = nil
			*currentConfig, err = sjson.Set(*currentConfig, jsonPath, value.Val())
			if err != nil {
				logger.Error("failed to reload config", zap.Error(err))
			}
			break
		case "rename_from", "del", "evicted", "expired":
			var err error = nil
			*currentConfig, err = sjson.Delete(*currentConfig, jsonPath)
			if err != nil {
				logger.Error("failed to reload config", zap.Error(err))
			}
			break
		}

		fmt.Printf(*currentConfig)
		err := caddy.Load([]byte(*currentConfig), true)
		if err != nil {
			logger.Error("failed to reload config", zap.Error(err))
		}
	}
}

func reloadConfig(ctx context.Context, client *redis.Client, keyPrefix string) error {
	config, err := loadConfigFromRedis(ctx, client, keyPrefix)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	err = caddy.Load([]byte(config), true)

	if err != nil {
		return fmt.Errorf("failed to reload caddy: %w", err)
	}

	return nil
}
