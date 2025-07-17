package interfaces

import (
	"slack-queue-bot/internal/models"
	"time"

	"github.com/slack-go/slack"
)

// QueueService defines the interface for queue management operations
type QueueService interface {
	JoinQueue(userID, username, environment, tag string, duration time.Duration) error
	LeaveQueue(userID string) error
	GetUserPosition(userID string) *models.UserPosition
	GetQueueStatus() *models.QueueStatus
	ProcessQueue() error
	IsQueueEmpty(environment, tag string) bool
}

// TagService defines the interface for tag management operations
type TagService interface {
	AssignTag(userID string, environment, tag string, duration time.Duration) (*models.Tag, error)
	ReleaseTag(userID, environment, tag string) error
	ExtendAssignment(userID, environment, tag string, extension time.Duration) error
	GetAvailableTag(environment string) (*models.Tag, error)
	GetUserAssignment(userID string) (*models.Tag, error)
	UpdateTagStatus(environment, tag, status string) error
	GetExpiredTags() []*models.Tag
	ReleaseExpiredTags() error
	GetUpcomingExpirations(within time.Duration) []*models.Tag
}

// NotificationService defines the interface for sending notifications
type NotificationService interface {
	NotifyUser(userID, message string) error
	NotifyUserBlock(userID, emoji, summary, detail, suggestion string) error
	BroadcastQueueUpdate() error
	CreateQueueStatusBlocks() ([]slack.Block, error)
	NotifyAssignment(userID string, tag *models.Tag, environment string) error
	NotifyExpiration(userID string, tag *models.Tag, environment string) error
	NotifyQueueJoined(userID string, position int) error
	NotifyQueueLeft(userID string) error
	NotifyTagRelease(userID string, environment, tag string) error
	NotifyAssignmentExtended(userID string, tag *models.Tag, environment string, extension time.Duration) error
	CreatePositionInfo(userPos *models.UserPosition, availableTags int) string
	CreateEnvironmentList(environments map[string]*models.Environment) string
	SetQueueService(queueService QueueService)
}

// ConfigService defines the interface for configuration management
type ConfigService interface {
	LoadConfig() (*models.Config, error)
	GetSettings() *models.ConfigSettings
	GetEnvironments() []models.ConfigEnvironment
	ValidateEnvironment(environment string) error
	ValidateTag(environment, tag string) error
	GetConfigSummary() map[string]interface{}
}

// PersistenceService defines the interface for data persistence
type PersistenceService interface {
	SaveState(data *models.PersistenceData) error
	LoadState() (*models.PersistenceData, error)
	InitializeEnvironments(environments []models.ConfigEnvironment) error
	AutoSave() error
}

// TTLService defines the interface for TTL-based expiration management
type TTLService interface {
	ProcessExpirations() error
	GetUpcomingExpirations(window time.Duration) ([]models.Tag, error)
	CleanupOldEvents() error
	StartBackgroundProcessor() error
	StopBackgroundProcessor() error
}

// SlackHandler defines the interface for Slack event handling
type SlackHandler interface {
	HandleAppMention(event *models.SlackEvent) (*models.CommandResponse, error)
	HandleInteraction(event *models.SlackEvent) (*models.CommandResponse, error)
	ParseCommand(text, userID, username, channelID string) (*models.CommandRequest, error)
	CreateQueueStatusBlocks() (interface{}, error) // returns Slack blocks
}

// HTTPHandler defines the interface for HTTP endpoint handling
type HTTPHandler interface {
	HealthCheck() (int, string)
	ManualSave() (int, string)
	GetStatus() (int, interface{})
}

// ValidationService defines the interface for input validation
type ValidationService interface {
	ValidateDuration(duration time.Duration) error
	ValidateQueueSize(currentSize int) error
	ValidateUserInQueue(userID string, queue []models.QueueItem) error
	ValidateEnvironmentExists(environment string, environments map[string]*models.Environment) error
	ValidateTagExists(environment, tag string, environments map[string]*models.Environment) error
	ValidateTagAvailability(environment, tag string, environments map[string]*models.Environment) error
	ValidateUserAssignment(userID, environment, tag string, environments map[string]*models.Environment) error
	ValidateCommand(cmd *models.CommandRequest) error
}

// DatabaseRepository defines the interface for database operations
type DatabaseRepository interface {
	// Environment operations
	CreateEnvironment(name string, tags []string) error
	GetEnvironments() (map[string]*models.Environment, error)

	// Tag operations
	UpdateTagStatus(environment, tag, status string, assignedTo *string, expiresAt *time.Time) error
	GetTag(environment, tag string) (*models.Tag, error)
	GetAvailableTags(environment string) ([]*models.Tag, error)

	// Queue operations
	JoinQueue(userID, username, environment, tag string, durationMinutes int) error
	LeaveQueue(userID string) error
	GetQueue() ([]models.QueueItem, error)
	GetUserPosition(userID string) (int, *models.QueueItem, error)
	IsQueueEmpty(environment, tag string) (bool, error)

	// TTL operations
	GetExpiredEvents() ([]models.Tag, error)
	ProcessExpirationEvent(tagID int) error
	GetUpcomingExpirations(window time.Duration) ([]models.Tag, error)
	CleanupProcessedEvents(cutoff time.Duration) error

	// Database-driven auto-expiration operations
	TriggerDatabaseAutoExpiration() (int, error)
	GetExpiredTagsFromView() ([]models.Tag, error)
	CleanupAutoExpirationLogs(olderThan time.Duration) error

	// Real-time expiration notification operations
	StartExpirationListener(notificationChan chan<- interface{}, dbPath string) error
	TriggerDatabaseAutoExpirationWithNotifications(notificationChan chan<- interface{}) (int, error)

	// Utility
	Close() error
}

// ApplicationService defines the main application service interface
type ApplicationService interface {
	Start() error
	Stop() error
	ProcessCommand(request *models.CommandRequest) (*models.CommandResponse, error)
	GetStatus() *models.QueueStatus
}
