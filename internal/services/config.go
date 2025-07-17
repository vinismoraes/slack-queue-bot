package services

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
)

// ConfigService handles configuration management
type ConfigService struct {
	config     *models.Config
	configPath string
}

// NewConfigService creates a new configuration service
func NewConfigService(configPath string) interfaces.ConfigService {
	return &ConfigService{
		configPath: configPath,
	}
}

// LoadConfig loads and validates the configuration from file
func (cs *ConfigService) LoadConfig() (*models.Config, error) {
	file, err := os.Open(cs.configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file %s: %w", cs.configPath, err)
	}
	defer file.Close()

	var config models.Config
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}

	// Set defaults if not specified
	cs.setDefaults(&config)

	// Validate configuration
	if err := cs.validateConfig(&config); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	cs.config = &config
	return &config, nil
}

// GetSettings returns the current configuration settings
func (cs *ConfigService) GetSettings() *models.ConfigSettings {
	if cs.config == nil {
		// Return defaults if config not loaded
		defaults := &models.ConfigSettings{}
		cs.setSettingsDefaults(defaults)
		return defaults
	}
	return &cs.config.Settings
}

// GetEnvironments returns the list of configured environments
func (cs *ConfigService) GetEnvironments() []models.ConfigEnvironment {
	if cs.config == nil {
		return []models.ConfigEnvironment{}
	}
	return cs.config.Environments
}

// ValidateEnvironment checks if an environment exists in the configuration
func (cs *ConfigService) ValidateEnvironment(environment string) error {
	if cs.config == nil {
		return fmt.Errorf("configuration not loaded")
	}

	for _, env := range cs.config.Environments {
		if env.Name == environment {
			return nil
		}
	}

	return fmt.Errorf("environment '%s' not found in configuration", environment)
}

// ValidateTag checks if a tag exists in a specific environment
func (cs *ConfigService) ValidateTag(environment, tag string) error {
	if cs.config == nil {
		return fmt.Errorf("configuration not loaded")
	}

	for _, env := range cs.config.Environments {
		if env.Name == environment {
			for _, tagName := range env.Tags {
				if tagName == tag {
					return nil
				}
			}
			return fmt.Errorf("tag '%s' not found in environment '%s'", tag, environment)
		}
	}

	return fmt.Errorf("environment '%s' not found in configuration", environment)
}

// ReloadConfig reloads the configuration from file
func (cs *ConfigService) ReloadConfig() error {
	_, err := cs.LoadConfig()
	return err
}

// GetEnvironmentTags returns all tags for a specific environment
func (cs *ConfigService) GetEnvironmentTags(environment string) ([]string, error) {
	if cs.config == nil {
		return nil, fmt.Errorf("configuration not loaded")
	}

	for _, env := range cs.config.Environments {
		if env.Name == environment {
			return env.Tags, nil
		}
	}

	return nil, fmt.Errorf("environment '%s' not found", environment)
}

// GetAllEnvironmentNames returns a list of all environment names
func (cs *ConfigService) GetAllEnvironmentNames() []string {
	if cs.config == nil {
		return []string{}
	}

	names := make([]string, len(cs.config.Environments))
	for i, env := range cs.config.Environments {
		names[i] = env.Name
	}
	return names
}

// GetTotalTagCount returns the total number of tags across all environments
func (cs *ConfigService) GetTotalTagCount() int {
	if cs.config == nil {
		return 0
	}

	total := 0
	for _, env := range cs.config.Environments {
		total += len(env.Tags)
	}
	return total
}

// setDefaults sets default values for configuration
func (cs *ConfigService) setDefaults(config *models.Config) {
	cs.setSettingsDefaults(&config.Settings)
}

// setSettingsDefaults sets default values for settings
func (cs *ConfigService) setSettingsDefaults(settings *models.ConfigSettings) {
	if settings.MaxQueueSize == 0 {
		settings.MaxQueueSize = 50
	}
	if settings.DefaultDuration == 0 {
		settings.DefaultDuration = models.Duration(3 * time.Hour)
	}
	if settings.MaxDuration == 0 {
		settings.MaxDuration = models.Duration(3 * time.Hour)
	}
	if settings.MinDuration == 0 {
		settings.MinDuration = models.Duration(30 * time.Minute)
	}
	if settings.ExtensionTime == 0 {
		settings.ExtensionTime = models.Duration(3 * time.Hour)
	}
	if settings.ExpirationCheck == 0 {
		settings.ExpirationCheck = models.Duration(5 * time.Minute)
	}
	if settings.AutoSaveInterval == 0 {
		settings.AutoSaveInterval = models.Duration(10 * time.Minute)
	}
}

// validateConfig validates the loaded configuration
func (cs *ConfigService) validateConfig(config *models.Config) error {
	// Validate environments
	if len(config.Environments) == 0 {
		return fmt.Errorf("at least one environment must be configured")
	}

	environmentNames := make(map[string]bool)
	for _, env := range config.Environments {
		// Check for duplicate environment names
		if environmentNames[env.Name] {
			return fmt.Errorf("duplicate environment name: %s", env.Name)
		}
		environmentNames[env.Name] = true

		// Validate environment name
		if env.Name == "" {
			return fmt.Errorf("environment name cannot be empty")
		}

		// Validate tags
		if len(env.Tags) == 0 {
			return fmt.Errorf("environment '%s' must have at least one tag", env.Name)
		}

		tagNames := make(map[string]bool)
		for _, tag := range env.Tags {
			// Check for duplicate tag names within environment
			if tagNames[tag] {
				return fmt.Errorf("duplicate tag name '%s' in environment '%s'", tag, env.Name)
			}
			tagNames[tag] = true

			// Validate tag name
			if tag == "" {
				return fmt.Errorf("tag name cannot be empty in environment '%s'", env.Name)
			}
		}
	}

	// Validate settings
	if err := cs.validateSettings(&config.Settings); err != nil {
		return fmt.Errorf("settings validation failed: %w", err)
	}

	return nil
}

// validateSettings validates configuration settings
func (cs *ConfigService) validateSettings(settings *models.ConfigSettings) error {
	if settings.MaxQueueSize <= 0 {
		return fmt.Errorf("max_queue_size must be greater than 0")
	}

	if settings.DefaultDuration.ToDuration() <= 0 {
		return fmt.Errorf("default_duration must be greater than 0")
	}

	if settings.MaxDuration.ToDuration() <= 0 {
		return fmt.Errorf("max_duration must be greater than 0")
	}

	if settings.MinDuration.ToDuration() <= 0 {
		return fmt.Errorf("min_duration must be greater than 0")
	}

	if settings.MinDuration.ToDuration() > settings.MaxDuration.ToDuration() {
		return fmt.Errorf("min_duration cannot be greater than max_duration")
	}

	if settings.DefaultDuration.ToDuration() > settings.MaxDuration.ToDuration() {
		return fmt.Errorf("default_duration cannot be greater than max_duration")
	}

	if settings.DefaultDuration.ToDuration() < settings.MinDuration.ToDuration() {
		return fmt.Errorf("default_duration cannot be less than min_duration")
	}

	if settings.ExtensionTime.ToDuration() <= 0 {
		return fmt.Errorf("extension_time must be greater than 0")
	}

	if settings.ExpirationCheck.ToDuration() <= 0 {
		return fmt.Errorf("expiration_check must be greater than 0")
	}

	if settings.AutoSaveInterval.ToDuration() <= 0 {
		return fmt.Errorf("auto_save_interval must be greater than 0")
	}

	// Validate duration intervals (should be in 30-minute increments)
	thirtyMinutes := 30 * time.Minute
	if settings.MinDuration.ToDuration()%thirtyMinutes != 0 {
		return fmt.Errorf("min_duration must be in 30-minute intervals")
	}

	if settings.MaxDuration.ToDuration()%thirtyMinutes != 0 {
		return fmt.Errorf("max_duration must be in 30-minute intervals")
	}

	if settings.DefaultDuration.ToDuration()%thirtyMinutes != 0 {
		return fmt.Errorf("default_duration must be in 30-minute intervals")
	}

	return nil
}

// GetConfigSummary returns a summary of the current configuration
func (cs *ConfigService) GetConfigSummary() map[string]interface{} {
	summary := map[string]interface{}{
		"config_loaded": cs.config != nil,
		"config_path":   cs.configPath,
	}

	if cs.config != nil {
		summary["environments"] = len(cs.config.Environments)
		summary["total_tags"] = cs.GetTotalTagCount()
		summary["settings"] = map[string]interface{}{
			"max_queue_size":     cs.config.Settings.MaxQueueSize,
			"default_duration":   cs.config.Settings.DefaultDuration.String(),
			"max_duration":       cs.config.Settings.MaxDuration.String(),
			"min_duration":       cs.config.Settings.MinDuration.String(),
			"extension_time":     cs.config.Settings.ExtensionTime.String(),
			"expiration_check":   cs.config.Settings.ExpirationCheck.String(),
			"auto_save_interval": cs.config.Settings.AutoSaveInterval.String(),
		}
	}

	return summary
}
