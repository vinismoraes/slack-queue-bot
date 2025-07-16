package models

import "time"

// Tag represents a bookable tag within an environment
type Tag struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"` // "available", "occupied", "maintenance"
	AssignedTo  string    `json:"assigned_to,omitempty"`
	AssignedAt  time.Time `json:"assigned_at,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	Environment string    `json:"environment"`
}

// Environment represents a test environment containing multiple tags
type Environment struct {
	Name string          `json:"name"`
	Tags map[string]*Tag `json:"tags"`
}

// QueueItem represents a user in the queue for a specific tag
type QueueItem struct {
	UserID      string        `json:"user_id"`
	Username    string        `json:"username"`
	JoinedAt    time.Time     `json:"joined_at"`
	Environment string        `json:"environment"`
	Tag         string        `json:"tag"`
	Duration    time.Duration `json:"duration"` // How long they need the tag for
	Priority    int           `json:"priority"` // Higher number = higher priority
}

// PersistenceData represents the complete state to be saved/loaded
type PersistenceData struct {
	Queue        []QueueItem             `json:"queue"`
	Environments map[string]*Environment `json:"environments"`
	LastSaved    time.Time               `json:"last_saved"`
	Version      string                  `json:"version"` // For migration support
}

// Config represents the configuration structure
type Config struct {
	Environments []struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	} `json:"environments"`
	Settings struct {
		MaxQueueSize     int           `json:"max_queue_size"`
		DefaultDuration  time.Duration `json:"default_duration"`
		MaxDuration      time.Duration `json:"max_duration"`
		MinDuration      time.Duration `json:"min_duration"`
		ExtensionTime    time.Duration `json:"extension_time"`
		ExpirationCheck  time.Duration `json:"expiration_check"`
		AutoSaveInterval time.Duration `json:"auto_save_interval"`
	} `json:"settings"`
}
