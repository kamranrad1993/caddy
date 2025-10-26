package redisadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func init() {
	caddyconfig.RegisterAdapter("redis", RedisAdapter{})
}

// RedisAdapter implements the Caddy config adapter interface
type RedisAdapter struct{}

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

	// Start pub/sub listener in background
	// go startPubSubListener(client, redisConfig.KeyPrefix, redisConfig.Channel)

	return config, nil, nil
}

// RedisConfig holds the Redis connection configuration
type RedisConfig struct {
	Addr      string `json:"addr"`
	Password  string `json:"password,omitempty"`
	DB        int    `json:"db"`
	KeyPrefix string `json:"key_prefix"`
	Channel   string `json:"channel"`
}

// getConfigFromEnv reads Redis config from environment variables
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

// ConfigManager handles configuration updates
type ConfigManager struct {
	client    *redis.Client
	keyPrefix string
	channel   string
	mu        sync.RWMutex
	logger    *zap.Logger
}

var globalManager *ConfigManager

// loadConfigFromRedis retrieves all config keys and reconstructs the JSON
func loadConfigFromRedis(ctx context.Context, client *redis.Client, keyPrefix string) ([]byte, error) {
	pattern := keyPrefix + ":*"
	var cursor uint64
	configMap := make(map[string]interface{})

	// Scan all keys matching the pattern
	for {
		keys, nextCursor, err := client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, fmt.Errorf("failed to scan keys: %w", err)
		}

		// Get values for all keys
		for _, key := range keys {
			val, err := client.Get(ctx, key).Result()
			if err != nil {
				continue // Skip missing keys
			}

			// Extract JSON path from key
			jsonPath := strings.TrimPrefix(key, keyPrefix+":")

			// Set value in config map using JSON path
			if err := setValueByPath(configMap, jsonPath, val); err != nil {
				return nil, fmt.Errorf("failed to set value for path %s: %w", jsonPath, err)
			}
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	// Marshal the reconstructed config
	return json.Marshal(configMap)
}

// setValueByPath sets a value in a nested map using a JSON path
func setValueByPath(m map[string]interface{}, path string, value string) error {
	parts := strings.Split(path, ".")
	current := m

	// Navigate/create nested structure
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]

		// Check if this part is an array index
		if strings.Contains(part, "[") && strings.Contains(part, "]") {
			// Handle array notation: routes[0]
			base := part[:strings.Index(part, "[")]
			indexStr := part[strings.Index(part, "[")+1 : strings.Index(part, "]")]
			var index int
			fmt.Sscanf(indexStr, "%d", &index)

			// Ensure array exists
			if _, exists := current[base]; !exists {
				current[base] = make([]interface{}, 0)
			}

			arr, ok := current[base].([]interface{})
			if !ok {
				return fmt.Errorf("expected array at %s", base)
			}

			// Extend array if needed
			for len(arr) <= index {
				arr = append(arr, make(map[string]interface{}))
			}
			current[base] = arr

			// Navigate into array element
			if i < len(parts)-2 {
				elem, ok := arr[index].(map[string]interface{})
				if !ok {
					elem = make(map[string]interface{})
					arr[index] = elem
				}
				current = elem
			}
		} else {
			if _, exists := current[part]; !exists {
				current[part] = make(map[string]interface{})
			}

			next, ok := current[part].(map[string]interface{})
			if !ok {
				return fmt.Errorf("path conflict at %s", part)
			}
			current = next
		}
	}

	// Set the final value
	lastKey := parts[len(parts)-1]

	// Try to parse as JSON first
	var parsedValue interface{}
	if err := json.Unmarshal([]byte(value), &parsedValue); err == nil {
		current[lastKey] = parsedValue
	} else {
		current[lastKey] = value
	}

	return nil
}

// startPubSubListener subscribes to Redis pub/sub for config changes
func startPubSubListener(client *redis.Client, keyPrefix, channel string) {
	ctx := context.Background()
	pubsub := client.Subscribe(ctx, channel)
	defer pubsub.Close()

	logger := caddy.Log().Named("redis-config-loader")
	logger.Info("started redis pub/sub listener", zap.String("channel", channel))

	globalManager = &ConfigManager{
		client:    client,
		keyPrefix: keyPrefix,
		channel:   channel,
		logger:    logger,
	}

	// Process messages
	ch := pubsub.Channel()
	for msg := range ch {
		logger.Info("received config update", zap.String("message", msg.Payload))

		// Small delay to allow multiple updates to complete
		time.Sleep(100 * time.Millisecond)

		// Reload configuration
		if err := reloadConfig(ctx, client, keyPrefix); err != nil {
			logger.Error("failed to reload config", zap.Error(err))
		} else {
			logger.Info("config reloaded successfully")
		}
	}
}

// reloadConfig rebuilds the entire config from Redis and reloads Caddy
func reloadConfig(ctx context.Context, client *redis.Client, keyPrefix string) error {
	// Rebuild config from Redis
	config, err := loadConfigFromRedis(ctx, client, keyPrefix)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Parse and validate config
	var cfg interface{}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	// Reload Caddy with new config
	err = caddy.Load(config, true)
	if err != nil {
		return fmt.Errorf("failed to reload caddy: %w", err)
	}

	return nil
}

// PublishConfigUpdate publishes a notification that config has changed
func PublishConfigUpdate(ctx context.Context, message string) error {
	if globalManager == nil {
		return fmt.Errorf("config manager not initialized")
	}

	return globalManager.client.Publish(ctx, globalManager.channel, message).Err()
}

// SetConfigValue sets a configuration value in Redis using JSON path
func SetConfigValue(ctx context.Context, jsonPath, value string) error {
	if globalManager == nil {
		return fmt.Errorf("config manager not initialized")
	}

	key := globalManager.keyPrefix + ":" + jsonPath

	if err := globalManager.client.Set(ctx, key, value, 0).Err(); err != nil {
		return fmt.Errorf("failed to set value: %w", err)
	}

	// Publish update notification
	return PublishConfigUpdate(ctx, fmt.Sprintf("updated:%s", jsonPath))
}

// DeleteConfigValue deletes a configuration value from Redis
func DeleteConfigValue(ctx context.Context, jsonPath string) error {
	if globalManager == nil {
		return fmt.Errorf("config manager not initialized")
	}

	key := globalManager.keyPrefix + ":" + jsonPath

	if err := globalManager.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete value: %w", err)
	}

	// Publish update notification
	return PublishConfigUpdate(ctx, fmt.Sprintf("deleted:%s", jsonPath))
}

// var _ caddy.Validator = (*RedisAdapter)(nil)
// var _ caddy.ConfigLoader = (*RedisAdapter)(nil)
