package redisadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

func init() {
	caddyconfig.RegisterAdapter("redis", RedisAdapter{})
}

// RedisAdapter is a Caddy config adapter that loads configuration from Redis.
type RedisAdapter struct{}

// ConfigManager handles thread-safe config updates and lifecycle
type ConfigManager struct {
	mu             sync.RWMutex
	config         string
	client         *redis.Client
	keyPrefix      string
	db             int
	cancel         context.CancelFunc
	logger         *zap.Logger
	reloadDebounce time.Duration
	lastReload     time.Time
	pendingReload  bool        // Track if a reload is pending
	reloadTimer    *time.Timer // Timer for pending reload
	timerMu        sync.Mutex  // Separate mutex for timer operations
}

// Adapt is called by Caddy when using --adapter redis.
func (a RedisAdapter) Adapt(body []byte, options map[string]interface{}) ([]byte, []caddyconfig.Warning, error) {
	ctx := context.Background()

	var redisConfig RedisConfig

	// Try to parse Redis connection info from the config file,
	// or fall back to environment variables if the file is empty
	if len(body) == 0 || string(body) == "{}" {
		redisConfig = getConfigFromEnv()
	} else {
		if err := json.Unmarshal(body, &redisConfig); err != nil {
			return nil, nil, fmt.Errorf("failed to parse redis config: %w", err)
		}
	}

	// Apply sensible defaults
	if redisConfig.Addr == "" {
		redisConfig.Addr = "localhost:6379"
	}
	if redisConfig.KeyPrefix == "" {
		redisConfig.KeyPrefix = "caddy:config"
	}
	if redisConfig.ReloadDebounce == 0 {
		redisConfig.ReloadDebounce = 1 * time.Second
	}

	client := redis.NewClient(&redis.Options{
		Addr:         redisConfig.Addr,
		Password:     redisConfig.Password,
		DB:           redisConfig.DB,
		PoolSize:     10,
		MinIdleConns: 2,
		MaxRetries:   3,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	// Make sure we can actually connect before proceeding
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("failed to connect to redis: %w", err)
	}

	// Load the initial configuration from Redis
	config, err := loadConfigFromRedis(ctx, client, redisConfig.KeyPrefix)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("failed to load config from redis: %w", err)
	}

	// Validate initial config
	if !json.Valid([]byte(config)) {
		client.Close()
		return nil, nil, fmt.Errorf("invalid JSON config loaded from Redis")
	}

	// Enable keyspace notifications so we can watch for changes.
	if err := enableKeyspaceNotifications(ctx, client); err != nil {
		log.Printf("Warning: Failed to set Redis keyspace events: %v", err)
	}

	// Create config manager with proper lifecycle management
	bgCtx, bgCancel := context.WithCancel(context.Background())
	manager := &ConfigManager{
		config:         config,
		client:         client,
		keyPrefix:      redisConfig.KeyPrefix,
		db:             redisConfig.DB,
		cancel:         bgCancel,
		logger:         caddy.Log().Named("redis-adapter"),
		reloadDebounce: redisConfig.ReloadDebounce * time.Second,
		pendingReload:  false,
	}

	// Start listening for config changes in the background
	go manager.startPubSubListener(bgCtx)

	// Register cleanup on Caddy shutdown
	caddy.OnExit(func(ctx context.Context) {
		manager.Shutdown()

	})

	return []byte(config), nil, nil
}

type RedisConfig struct {
	Addr           string        `json:"addr"`
	Password       string        `json:"password,omitempty"`
	DB             int           `json:"db"`
	KeyPrefix      string        `json:"key_prefix"`
	ReloadDebounce time.Duration `json:"reload_debounce,omitempty"`
}

func getConfigFromEnv() RedisConfig {
	cfg := RedisConfig{
		Addr:      os.Getenv("REDIS_ADDR"),
		Password:  os.Getenv("REDIS_PASSWORD"),
		KeyPrefix: os.Getenv("REDIS_KEY_PREFIX"),
	}

	if dbStr := os.Getenv("REDIS_DB"); dbStr != "" {
		fmt.Sscanf(dbStr, "%d", &cfg.DB)
	}

	if debounceStr := os.Getenv("REDIS_RELOAD_DEBOUNCE"); debounceStr != "" {
		if d, err := time.ParseDuration(debounceStr); err == nil {
			cfg.ReloadDebounce = d
		}
	}

	return cfg
}

// enableKeyspaceNotifications configures Redis to send keyspace events
func enableKeyspaceNotifications(ctx context.Context, client *redis.Client) error {
	return client.ConfigSet(ctx, "notify-keyspace-events", "KEA").Err()
}

// isValidJSON does a quick check to see if a string looks like JSON
func isValidJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}

	firstChar := s[0]
	if firstChar != '{' && firstChar != '[' && firstChar != '"' &&
		s != "true" && s != "false" && s != "null" {
		return false
	}

	var js interface{}
	return json.Unmarshal([]byte(s), &js) == nil
}

// setValueInConfig intelligently sets a value in the JSON config.
func setValueInConfig(config, jsonPath, value string) (string, error) {
	value = strings.TrimSpace(value)

	if isValidJSON(value) {
		// SetRaw preserves the JSON structure exactly as-is
		return sjson.SetRaw(config, jsonPath, value)
	}

	// Set automatically quotes plain strings for us
	return sjson.Set(config, jsonPath, value)
}

// cleanupEmptyParents removes empty parent objects/arrays after a key deletion.
// It traverses up the JSON path and removes empty containers.
func cleanupEmptyParents(config, jsonPath string) (string, error) {
	result := config

	// Parse the JSON path to get parent paths
	parts := strings.Split(jsonPath, ".")

	// Iterate backwards through the path hierarchy to clean up empty parents
	for i := len(parts) - 1; i >= 0; i-- {
		// Build current parent path
		parentPath := strings.Join(parts[:i], ".")
		if parentPath == "" {
			break
		}

		// Get the parent value and check if it's empty
		parentResult := gjson.Get(result, parentPath)
		if !parentResult.Exists() {
			continue
		}

		// Check if parent is empty object or array
		if parentResult.IsObject() && len(parentResult.Map()) == 0 {
			// Parent is an empty object, remove it
			var err error
			result, err = sjson.Delete(result, parentPath)
			if err != nil {
				return result, fmt.Errorf("failed to delete empty object at %s: %w", parentPath, err)
			}
		} else if parentResult.IsArray() && len(parentResult.Array()) == 0 {
			// Parent is an empty array, remove it
			var err error
			result, err = sjson.Delete(result, parentPath)
			if err != nil {
				return result, fmt.Errorf("failed to delete empty array at %s: %w", parentPath, err)
			}
		}
	}

	return result, nil
}

// loadConfigFromRedis scans all keys matching our prefix and rebuilds
// the complete JSON config from scratch.
func loadConfigFromRedis(ctx context.Context, client *redis.Client, keyPrefix string) (string, error) {
	pattern := keyPrefix + ":*"
	var cursor uint64
	config := "{}"

	// SCAN is better than KEYS for production - it doesn't block Redis
	for {
		keys, nextCursor, err := client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return "", fmt.Errorf("failed to scan keys: %w", err)
		}

		// Batch GET operations for better performance
		if len(keys) > 0 {
			pipe := client.Pipeline()
			cmds := make(map[string]*redis.StringCmd, len(keys))

			for _, key := range keys {
				cmds[key] = pipe.Get(ctx, key)
			}

			if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
				// Continue even if some keys fail
				log.Printf("Warning: Pipeline exec error: %v", err)
			}

			for key, cmd := range cmds {
				val, err := cmd.Result()
				if err != nil {
					continue // Key might have been deleted
				}

				// Remove the prefix to get the JSON path
				jsonPath := strings.TrimPrefix(key, keyPrefix+":")

				config, err = setValueInConfig(config, jsonPath, val)
				if err != nil {
					return "", fmt.Errorf("failed to set value for path %s: %w", jsonPath, err)
				}
			}
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	return config, nil
}

// GetConfig returns a thread-safe copy of the current config
func (cm *ConfigManager) GetConfig() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.config
}

// UpdateConfig updates the config in a thread-safe manner
func (cm *ConfigManager) UpdateConfig(newConfig string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.config = newConfig
}

// scheduleReload schedules a reload after the debounce period
// This ensures pending changes are applied even if no new events arrive
func (cm *ConfigManager) scheduleReload() {
	cm.timerMu.Lock()
	defer cm.timerMu.Unlock()

	now := time.Now()
	timeSinceLastReload := now.Sub(cm.lastReload)

	// If we're within the debounce window, schedule a reload
	if timeSinceLastReload < cm.reloadDebounce {
		cm.pendingReload = true

		// Cancel existing timer if any
		if cm.reloadTimer != nil {
			cm.reloadTimer.Stop()
		}

		// Schedule reload after remaining debounce time
		remainingTime := cm.reloadDebounce - timeSinceLastReload
		cm.reloadTimer = time.AfterFunc(remainingTime, func() {
			cm.executePendingReload()
		})

		cm.logger.Debug("scheduled pending reload",
			zap.Duration("in", remainingTime))
	} else {
		// We can reload immediately
		cm.mu.Lock()
		cm.lastReload = now
		cm.mu.Unlock()

		cm.reloadCaddy(cm.GetConfig())
	}
}

// executePendingReload executes a pending reload if one exists
func (cm *ConfigManager) executePendingReload() {
	cm.timerMu.Lock()
	defer cm.timerMu.Unlock()

	if !cm.pendingReload {
		return
	}

	cm.pendingReload = false
	cm.mu.Lock()
	cm.lastReload = time.Now()
	cm.mu.Unlock()

	cm.logger.Info("executing pending reload")
	cm.reloadCaddy(cm.GetConfig())
}

// startPubSubListener watches Redis keyspace events and applies changes in real-time
func (cm *ConfigManager) startPubSubListener(ctx context.Context) {
	keyspacePrefix := fmt.Sprintf("__keyspace@%d__:", cm.db)
	pattern := keyspacePrefix + cm.keyPrefix + ":*"
	prefix := keyspacePrefix + cm.keyPrefix + ":"

	cm.logger.Info("started redis keyspace listener",
		zap.String("pattern", pattern))

	for {
		select {
		case <-ctx.Done():
			cm.logger.Info("stopping redis keyspace listener")
			return
		default:
		}

		pubsub := cm.client.PSubscribe(ctx, pattern)
		ch := pubsub.Channel()

		func() {
			defer pubsub.Close()

			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-ch:
					if !ok {
						cm.logger.Warn("pubsub channel closed, reconnecting...")
						return
					}

					cm.handleKeyspaceEvent(ctx, msg, prefix)
				}
			}
		}()

		// Reconnection backoff
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			cm.logger.Info("attempting to reconnect pubsub...")
		}
	}
}

// handleKeyspaceEvent processes individual keyspace events
func (cm *ConfigManager) handleKeyspaceEvent(ctx context.Context, msg *redis.Message, prefix string) {
	key := strings.TrimPrefix(msg.Channel, "__keyspace@"+fmt.Sprintf("%d", cm.db)+"__:")
	jsonPath := strings.TrimPrefix(msg.Channel, prefix)

	cm.logger.Info("keyspace event",
		zap.String("event", msg.Payload),
		zap.String("key", key),
		zap.String("path", jsonPath))

	currentConfig := cm.GetConfig()
	var newConfig string
	var err error

	switch msg.Payload {
	case "set", "rename_to":
		// Someone updated a key - grab the new value and apply it
		value, getErr := cm.client.Get(ctx, key).Result()
		if getErr != nil {
			cm.logger.Error("failed to get value", zap.Error(getErr))
			return
		}

		newConfig, err = setValueInConfig(currentConfig, jsonPath, value)

		cm.logger.Info(msg.Payload,
			zap.String("jsonPath", jsonPath),
			zap.String("value", value))

		if err != nil {
			cm.logger.Error("failed to set config value",
				zap.String("path", jsonPath),
				zap.Error(err))
			return
		}

	case "rename_from", "del", "evicted", "expired":
		// Key was removed - delete it from our config too
		newConfig, err = sjson.Delete(currentConfig, jsonPath)

		if err != nil {
			cm.logger.Error("failed to delete config value",
				zap.String("path", jsonPath),
				zap.Error(err))
			return
		}

		// Clean up any empty parent objects that might result from the deletion
		newConfig, err = cleanupEmptyParents(newConfig, jsonPath)
		if err != nil {
			cm.logger.Error("failed to clean up empty parents after deletion",
				zap.String("path", jsonPath),
				zap.Error(err))
			// Continue with the config even if cleanup fails, to avoid losing the main deletion
		}

		cm.logger.Info(msg.Payload,
			zap.String("jsonPath", jsonPath))

	default:
		// Ignore other events
		return
	}

	// Update the stored config
	cm.UpdateConfig(newConfig)

	// Schedule reload with debouncing
	// This will ensure the reload happens even if no new events arrive
	cm.scheduleReload()
}

// reloadCaddy validates and applies the new configuration
func (cm *ConfigManager) reloadCaddy(config string) {
	cm.logger.Info("reloading caddy configuration")
	cm.logger.Info(config)

	// Double-check the JSON is valid before trying to load it
	if !json.Valid([]byte(config)) {
		cm.logger.Error("invalid JSON config after update, skipping reload")
		return
	}

	// Tell Caddy to apply the new config
	err := caddy.Load([]byte(config), true)
	if err != nil {
		cm.logger.Error("failed to reload caddy", zap.Error(err))
	} else {
		cm.logger.Info("caddy configuration reloaded successfully")
	}
}

// Shutdown gracefully stops the config manager
func (cm *ConfigManager) Shutdown() {
	cm.logger.Info("shutting down redis adapter")

	// Stop any pending reload timer
	cm.timerMu.Lock()
	if cm.reloadTimer != nil {
		cm.reloadTimer.Stop()
	}
	cm.timerMu.Unlock()

	if cm.cancel != nil {
		cm.cancel()
	}

	if cm.client != nil {
		if err := cm.client.Close(); err != nil {
			cm.logger.Error("error closing redis client", zap.Error(err))
		}
	}
}
