package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"slack-queue-bot/internal/models"
)

// Manager handles configuration loading and validation
type Manager struct {
	config *models.Config
	path   string
}

// NewManager creates a new configuration manager
func NewManager(configPath string) *Manager {
	return &Manager{
		path: configPath,
	}
}

// Load loads and validates the configuration
func (cm *Manager) Load() error {
	file, err := os.Open(cm.path)
	if err != nil {
		return fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	var config models.Config
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("failed to decode config: %w", err)
	}

	// Set defaults if not specified
	cm.setDefaults(&config)

	// Validate configuration
	if err := cm.validate(&config); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}

	cm.config = &config
	return nil
}

// Get returns the loaded configuration
func (cm *Manager) Get() *models.Config {
	return cm.config
}

// setDefaults sets default values for configuration
func (cm *Manager) setDefaults(config *models.Config) {
	if config.Settings.MaxQueueSize == 0 {
		config.Settings.MaxQueueSize = 50
	}
	if config.Settings.DefaultDuration == 0 {
		config.Settings.DefaultDuration = 3 * time.Hour
	}
	if config.Settings.MaxDuration == 0 {
		config.Settings.MaxDuration = 3 * time.Hour
	}
	if config.Settings.MinDuration == 0 {
		config.Settings.MinDuration = 30 * time.Minute
	}
	if config.Settings.ExtensionTime == 0 {
		config.Settings.ExtensionTime = 3 * time.Hour
	}
	if config.Settings.ExpirationCheck == 0 {
		config.Settings.ExpirationCheck = 5 * time.Minute
	}
	if config.Settings.AutoSaveInterval == 0 {
		config.Settings.AutoSaveInterval = 10 * time.Minute
	}
}

// validate validates the configuration
func (cm *Manager) validate(config *models.Config) error {
	if len(config.Environments) == 0 {
		return fmt.Errorf("no environments defined")
	}

	// Check for duplicate environment names
	envNames := make(map[string]bool)
	for _, env := range config.Environments {
		if env.Name == "" {
			return fmt.Errorf("environment name cannot be empty")
		}
		if envNames[env.Name] {
			return fmt.Errorf("duplicate environment name: %s", env.Name)
		}
		envNames[env.Name] = true

		if len(env.Tags) == 0 {
			return fmt.Errorf("environment %s has no tags", env.Name)
		}

		// Check for duplicate tag names within environment
		tagNames := make(map[string]bool)
		for _, tag := range env.Tags {
			if tag == "" {
				return fmt.Errorf("tag name cannot be empty in environment %s", env.Name)
			}
			if tagNames[tag] {
				return fmt.Errorf("duplicate tag name %s in environment %s", tag, env.Name)
			}
			tagNames[tag] = true
		}
	}

	// Validate settings
	if config.Settings.MaxQueueSize <= 0 {
		return fmt.Errorf("max_queue_size must be positive")
	}
	if config.Settings.MinDuration <= 0 {
		return fmt.Errorf("min_duration must be positive")
	}
	if config.Settings.MaxDuration <= 0 {
		return fmt.Errorf("max_duration must be positive")
	}
	if config.Settings.DefaultDuration <= 0 {
		return fmt.Errorf("default_duration must be positive")
	}
	if config.Settings.MinDuration > config.Settings.MaxDuration {
		return fmt.Errorf("min_duration cannot be greater than max_duration")
	}
	if config.Settings.DefaultDuration < config.Settings.MinDuration || config.Settings.DefaultDuration > config.Settings.MaxDuration {
		return fmt.Errorf("default_duration must be between min_duration and max_duration")
	}

	return nil
}
