package models

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a custom type that wraps time.Duration to support JSON unmarshaling from strings
type Duration time.Duration

// UnmarshalJSON implements the json.Unmarshaler interface for Duration
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	duration, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration format %q: %w", s, err)
	}

	*d = Duration(duration)
	return nil
}

// MarshalJSON implements the json.Marshaler interface for Duration
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// String returns the string representation of the duration
func (d Duration) String() string {
	return time.Duration(d).String()
}

// ToDuration converts the custom Duration to time.Duration
func (d Duration) ToDuration() time.Duration {
	return time.Duration(d)
}

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
	Environments []ConfigEnvironment `json:"environments"`
	Settings     ConfigSettings      `json:"settings"`
}

// ConfigEnvironment represents environment configuration
type ConfigEnvironment struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// ConfigSettings represents application settings
type ConfigSettings struct {
	MaxQueueSize     int      `json:"max_queue_size"`
	DefaultDuration  Duration `json:"default_duration"`
	MaxDuration      Duration `json:"max_duration"`
	MinDuration      Duration `json:"min_duration"`
	ExtensionTime    Duration `json:"extension_time"`
	ExpirationCheck  Duration `json:"expiration_check"`
	AutoSaveInterval Duration `json:"auto_save_interval"`
}

// QueueStatus represents the current status of the queue and environments
type QueueStatus struct {
	Queue         []QueueItem             `json:"queue"`
	Environments  map[string]*Environment `json:"environments"`
	TotalUsers    int                     `json:"total_users"`
	AvailableTags int                     `json:"available_tags"`
	OccupiedTags  int                     `json:"occupied_tags"`
}

// UserPosition represents a user's position information in the queue
type UserPosition struct {
	Position         int           `json:"position"`          // 0 = not in queue, -1 = has assignment
	QueueItem        *QueueItem    `json:"queue_item"`        // nil if not in queue
	EstimatedWait    time.Duration `json:"estimated_wait"`    // estimated wait time
	ActiveAssignment *Tag          `json:"active_assignment"` // nil if no assignment
}

// NotificationMessage represents a notification to be sent
type NotificationMessage struct {
	UserID     string `json:"user_id"`
	Message    string `json:"message"`
	Emoji      string `json:"emoji,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
	IsBlock    bool   `json:"is_block"` // true for block messages, false for simple text
}

// SlackEvent represents incoming Slack events
type SlackEvent struct {
	Type      string                 `json:"type"`
	UserID    string                 `json:"user_id"`
	ChannelID string                 `json:"channel_id"`
	Text      string                 `json:"text"`
	Timestamp string                 `json:"timestamp"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// CommandRequest represents a parsed command request
type CommandRequest struct {
	Command     string        `json:"command"`
	UserID      string        `json:"user_id"`
	Username    string        `json:"username"`
	ChannelID   string        `json:"channel_id"`
	Environment string        `json:"environment,omitempty"`
	Tag         string        `json:"tag,omitempty"`
	Duration    time.Duration `json:"duration,omitempty"`
	Arguments   []string      `json:"arguments,omitempty"`
}

// CommandResponse represents the response to a command
type CommandResponse struct {
	Success      bool   `json:"success"`
	Message      string `json:"message"`
	Error        string `json:"error,omitempty"`
	ShouldNotify bool   `json:"should_notify"` // whether to send notifications
	IsEphemeral  bool   `json:"is_ephemeral"`  // whether to send as ephemeral message
}

// ExpirationNotification represents a notification for tag expiration
type ExpirationNotification struct {
	Type        string    `json:"type"` // "tag_expired", "queue_processed", etc.
	TagID       int       `json:"tag_id"`
	Environment string    `json:"environment"`
	TagName     string    `json:"tag_name"`
	UserID      string    `json:"user_id,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}
