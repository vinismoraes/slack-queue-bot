package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"slack-queue-bot/internal/database"
	"slack-queue-bot/internal/models"
)

// Manager handles configuration loading and validation from database
type Manager struct {
	config *models.Config
	db     *database.DB
}

// NewManager creates a new configuration manager that loads from database
func NewManager(db *database.DB) *Manager {
	return &Manager{
		db: db,
	}
}

// NewManagerFromJSON creates a config manager from JSON (for migration purposes)
func NewManagerFromJSON(configPath string) *Manager {
	return &Manager{
		config: nil, // Will be loaded from JSON
	}
}

// SetDatabase sets the database instance for migration operations
func (cm *Manager) SetDatabase(db *database.DB) {
	cm.db = db
}

// GetConfigSummary returns a summary of the current configuration
func (cm *Manager) GetConfigSummary() map[string]interface{} {
	if cm.config == nil {
		return map[string]interface{}{
			"error": "configuration not loaded",
		}
	}

	return map[string]interface{}{
		"environments":       len(cm.config.Environments),
		"max_queue_size":     cm.config.Settings.MaxQueueSize,
		"default_duration":   cm.config.Settings.DefaultDuration.String(),
		"max_duration":       cm.config.Settings.MaxDuration.String(),
		"min_duration":       cm.config.Settings.MinDuration.String(),
		"extension_time":     cm.config.Settings.ExtensionTime.String(),
		"expiration_check":   cm.config.Settings.ExpirationCheck.String(),
		"auto_save_interval": cm.config.Settings.AutoSaveInterval.String(),
	}
}

// GetSettings returns the configuration settings
func (cm *Manager) GetSettings() *models.ConfigSettings {
	if cm.config == nil {
		return nil
	}
	return &cm.config.Settings
}

// LoadConfig loads and returns the configuration (compatibility method)
func (cm *Manager) LoadConfig() (*models.Config, error) {
	if err := cm.Load(); err != nil {
		return nil, err
	}
	return cm.config, nil
}

// GetEnvironments returns the list of configured environments
func (cm *Manager) GetEnvironments() []models.ConfigEnvironment {
	if cm.config == nil {
		return nil
	}
	return cm.config.Environments
}

// ValidateEnvironment checks if an environment exists
func (cm *Manager) ValidateEnvironment(environment string) error {
	if cm.config == nil {
		return fmt.Errorf("configuration not loaded")
	}

	for _, env := range cm.config.Environments {
		if env.Name == environment {
			return nil
		}
	}
	return fmt.Errorf("environment '%s' not found", environment)
}

// ValidateTag checks if a tag exists in the specified environment
func (cm *Manager) ValidateTag(environment, tag string) error {
	if cm.config == nil {
		return fmt.Errorf("configuration not loaded")
	}

	for _, env := range cm.config.Environments {
		if env.Name == environment {
			for _, envTag := range env.Tags {
				if envTag == tag {
					return nil
				}
			}
			return fmt.Errorf("tag '%s' not found in environment '%s'", tag, environment)
		}
	}
	return fmt.Errorf("environment '%s' not found", environment)
}

// Load loads and validates the configuration from database
func (cm *Manager) Load() error {
	// Load environments from database
	environments, err := cm.db.GetEnvironments()
	if err != nil {
		return fmt.Errorf("failed to get environments from database: %w", err)
	}

	// Convert database environments to config format
	var configEnvironments []models.ConfigEnvironment
	for _, env := range environments {
		tags, err := cm.db.GetTagsByEnvironment(env.Name)
		if err != nil {
			return fmt.Errorf("failed to get tags for environment %s: %w", env.Name, err)
		}

		var tagNames []string
		for _, tag := range tags {
			tagNames = append(tagNames, tag.Name)
		}

		configEnvironments = append(configEnvironments, models.ConfigEnvironment{
			Name:        env.Name,
			DisplayName: env.DisplayName,
			Tags:        tagNames,
		})
	}

	// Load settings from database
	dbSettings, err := cm.db.GetConfigSettings()
	if err != nil {
		return fmt.Errorf("failed to get config settings from database: %w", err)
	}

	// Parse settings into ConfigSettings struct
	settings, err := cm.parseSettings(dbSettings)
	if err != nil {
		return fmt.Errorf("failed to parse settings: %w", err)
	}

	// Create config
	config := &models.Config{
		Environments: configEnvironments,
		Settings:     *settings,
	}

	// Set defaults if not specified
	cm.setDefaults(config)

	// Validate configuration
	if err := cm.validate(config); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}

	cm.config = config
	return nil
}

// LoadFromJSON loads configuration from JSON file (for migration)
func (cm *Manager) LoadFromJSON(configPath string) error {
	file, err := os.Open(configPath)
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

// parseSettings converts database settings to ConfigSettings struct
func (cm *Manager) parseSettings(dbSettings map[string]database.ConfigSetting) (*models.ConfigSettings, error) {
	settings := &models.ConfigSettings{}

	for key, setting := range dbSettings {
		switch key {
		case "max_queue_size":
			var maxQueueSize int
			if _, err := fmt.Sscanf(setting.SettingValue, "%d", &maxQueueSize); err != nil {
				return nil, fmt.Errorf("invalid max_queue_size value: %s", setting.SettingValue)
			}
			settings.MaxQueueSize = maxQueueSize

		case "default_duration":
			duration, err := time.ParseDuration(setting.SettingValue)
			if err != nil {
				return nil, fmt.Errorf("invalid default_duration value: %s", setting.SettingValue)
			}
			settings.DefaultDuration = models.Duration(duration)

		case "max_duration":
			duration, err := time.ParseDuration(setting.SettingValue)
			if err != nil {
				return nil, fmt.Errorf("invalid max_duration value: %s", setting.SettingValue)
			}
			settings.MaxDuration = models.Duration(duration)

		case "min_duration":
			duration, err := time.ParseDuration(setting.SettingValue)
			if err != nil {
				return nil, fmt.Errorf("invalid min_duration value: %s", setting.SettingValue)
			}
			settings.MinDuration = models.Duration(duration)

		case "extension_time":
			duration, err := time.ParseDuration(setting.SettingValue)
			if err != nil {
				return nil, fmt.Errorf("invalid extension_time value: %s", setting.SettingValue)
			}
			settings.ExtensionTime = models.Duration(duration)

		case "expiration_check":
			duration, err := time.ParseDuration(setting.SettingValue)
			if err != nil {
				return nil, fmt.Errorf("invalid expiration_check value: %s", setting.SettingValue)
			}
			settings.ExpirationCheck = models.Duration(duration)

		case "auto_save_interval":
			duration, err := time.ParseDuration(setting.SettingValue)
			if err != nil {
				return nil, fmt.Errorf("invalid auto_save_interval value: %s", setting.SettingValue)
			}
			settings.AutoSaveInterval = models.Duration(duration)

		case "admin_users":
			var adminUsers []string
			if setting.SettingValue != "" && setting.SettingValue != "[]" {
				if err := json.Unmarshal([]byte(setting.SettingValue), &adminUsers); err != nil {
					return nil, fmt.Errorf("invalid admin_users value: %s", setting.SettingValue)
				}
			}
			settings.AdminUsers = adminUsers
		}
	}

	return settings, nil
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
		config.Settings.DefaultDuration = models.Duration(3 * time.Hour)
	}
	if config.Settings.MaxDuration == 0 {
		config.Settings.MaxDuration = models.Duration(3 * time.Hour)
	}
	if config.Settings.MinDuration == 0 {
		config.Settings.MinDuration = models.Duration(30 * time.Minute)
	}
	if config.Settings.ExtensionTime == 0 {
		config.Settings.ExtensionTime = models.Duration(3 * time.Hour)
	}
	if config.Settings.ExpirationCheck == 0 {
		config.Settings.ExpirationCheck = models.Duration(5 * time.Minute)
	}
	if config.Settings.AutoSaveInterval == 0 {
		config.Settings.AutoSaveInterval = models.Duration(10 * time.Minute)
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
	if config.Settings.MinDuration.ToDuration() <= 0 {
		return fmt.Errorf("min_duration must be positive")
	}
	if config.Settings.MaxDuration.ToDuration() <= 0 {
		return fmt.Errorf("max_duration must be positive")
	}
	if config.Settings.DefaultDuration.ToDuration() <= 0 {
		return fmt.Errorf("default_duration must be positive")
	}
	if config.Settings.MinDuration.ToDuration() > config.Settings.MaxDuration.ToDuration() {
		return fmt.Errorf("min_duration cannot be greater than max_duration")
	}
	if config.Settings.DefaultDuration.ToDuration() < config.Settings.MinDuration.ToDuration() || config.Settings.DefaultDuration.ToDuration() > config.Settings.MaxDuration.ToDuration() {
		return fmt.Errorf("default_duration must be between min_duration and max_duration")
	}

	return nil
}

// MigrateFromJSON migrates configuration from JSON to database
func (cm *Manager) MigrateFromJSON(configPath string) error {
	// Load from JSON first
	if err := cm.LoadFromJSON(configPath); err != nil {
		return fmt.Errorf("failed to load JSON config: %w", err)
	}

	config := cm.config
	if config == nil {
		return fmt.Errorf("no config loaded to migrate")
	}

	// Migrate environments and tags
	for _, env := range config.Environments {
		err := cm.db.CreateEnvironment(env.Name, env.Tags)
		if err != nil {
			// Log but don't fail on duplicate environments
			fmt.Printf("Warning: failed to create environment %s: %v\n", env.Name, err)
		}
	}

	// Migrate settings
	settingsMap := map[string]struct {
		value       string
		settingType string
		description string
	}{
		"max_queue_size":     {fmt.Sprintf("%d", config.Settings.MaxQueueSize), "integer", "Maximum number of users allowed in queue"},
		"default_duration":   {config.Settings.DefaultDuration.String(), "duration", "Default tag assignment duration"},
		"max_duration":       {config.Settings.MaxDuration.String(), "duration", "Maximum allowed tag assignment duration"},
		"min_duration":       {config.Settings.MinDuration.String(), "duration", "Minimum allowed tag assignment duration"},
		"extension_time":     {config.Settings.ExtensionTime.String(), "duration", "Duration to extend assignments by"},
		"expiration_check":   {config.Settings.ExpirationCheck.String(), "duration", "How often to check for expired assignments"},
		"auto_save_interval": {config.Settings.AutoSaveInterval.String(), "duration", "How often to auto-save data"},
	}

	for key, setting := range settingsMap {
		err := cm.db.SetConfigSetting(key, setting.value, setting.settingType, setting.description)
		if err != nil {
			return fmt.Errorf("failed to migrate setting %s: %w", key, err)
		}
	}

	fmt.Printf("Successfully migrated configuration from %s to database\n", configPath)
	return nil
}

// IsAdmin checks if a user ID has admin privileges
func (cm *Manager) IsAdmin(userID string) bool {
	if cm.config == nil {
		return false
	}

	for _, adminID := range cm.config.Settings.AdminUsers {
		if adminID == userID {
			return true
		}
	}
	return false
}

// AddAdmin adds a user to the admin list
func (cm *Manager) AddAdmin(userID string) error {
	if cm.config == nil {
		return fmt.Errorf("configuration not loaded")
	}

	// Check if already admin
	if cm.IsAdmin(userID) {
		return fmt.Errorf("user %s is already an admin", userID)
	}

	// Add to admin list
	cm.config.Settings.AdminUsers = append(cm.config.Settings.AdminUsers, userID)

	// Save to database
	return cm.saveAdminUsers()
}

// RemoveAdmin removes a user from the admin list
func (cm *Manager) RemoveAdmin(userID string) error {
	if cm.config == nil {
		return fmt.Errorf("configuration not loaded")
	}

	// Find and remove admin
	for i, adminID := range cm.config.Settings.AdminUsers {
		if adminID == userID {
			// Remove from slice
			cm.config.Settings.AdminUsers = append(cm.config.Settings.AdminUsers[:i], cm.config.Settings.AdminUsers[i+1:]...)

			// Save to database
			return cm.saveAdminUsers()
		}
	}

	return fmt.Errorf("user %s is not an admin", userID)
}

// GetAdmins returns the list of admin user IDs
func (cm *Manager) GetAdmins() []string {
	if cm.config == nil {
		return []string{}
	}
	return cm.config.Settings.AdminUsers
}

// saveAdminUsers saves the admin users list to the database
func (cm *Manager) saveAdminUsers() error {
	if cm.db == nil {
		return fmt.Errorf("database not available")
	}

	// Convert admin list to JSON
	adminJSON, err := json.Marshal(cm.config.Settings.AdminUsers)
	if err != nil {
		return fmt.Errorf("failed to marshal admin users: %w", err)
	}

	// Update in database using SetConfigSetting
	err = cm.db.SetConfigSetting("admin_users", string(adminJSON), "string", "JSON array of Slack user IDs with admin privileges")
	if err != nil {
		return fmt.Errorf("failed to save admin users to database: %w", err)
	}

	return nil
}
