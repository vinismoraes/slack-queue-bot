package database

import (
	"fmt"
	"time"

	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
)

// Repository implements the DatabaseRepository interface
type Repository struct {
	db *DB
}

// NewRepository creates a new database repository
func NewRepository(db *DB) interfaces.DatabaseRepository {
	return &Repository{db: db}
}

// convertDBTagToModel converts a database Tag to a models Tag
func convertDBTagToModel(dbTag Tag, environment string) *models.Tag {
	var assignedTo string
	var assignedAt, expiresAt time.Time

	if dbTag.AssignedTo != nil {
		assignedTo = *dbTag.AssignedTo
	}
	if dbTag.AssignedAt != nil {
		assignedAt = *dbTag.AssignedAt
	}
	if dbTag.ExpiresAt != nil {
		expiresAt = *dbTag.ExpiresAt
	}

	return &models.Tag{
		Name:        dbTag.Name,
		Status:      dbTag.Status,
		AssignedTo:  assignedTo,
		AssignedAt:  assignedAt,
		ExpiresAt:   expiresAt,
		Environment: environment,
	}
}

// Environment operations

// CreateEnvironment creates a new environment with its tags
func (r *Repository) CreateEnvironment(name string, tags []string) error {
	return r.db.CreateEnvironment(name, tags)
}

// GetEnvironments returns all environments with their tags as models
func (r *Repository) GetEnvironments() (map[string]*models.Environment, error) {
	environments, err := r.db.GetEnvironments()
	if err != nil {
		return nil, err
	}

	result := make(map[string]*models.Environment)
	for _, env := range environments {
		tags, err := r.db.GetTagsByEnvironment(env.Name)
		if err != nil {
			continue // Skip environments with tag errors
		}

		modelEnv := &models.Environment{
			Name:        env.Name,
			DisplayName: env.DisplayName,
			Tags:        make(map[string]*models.Tag),
		}

		for _, tag := range tags {
			modelTag := convertDBTagToModel(*tag, env.Name)
			modelEnv.Tags[tag.Name] = modelTag
		}

		result[env.Name] = modelEnv
	}

	return result, nil
}

// Tag operations

// UpdateTagStatus updates the status of a tag
func (r *Repository) UpdateTagStatus(environment, tag, status string, assignedTo *string, expiresAt *time.Time) error {
	return r.db.UpdateTagStatus(environment, tag, status, assignedTo, expiresAt)
}

// GetTag returns a specific tag by looking through all tags
func (r *Repository) GetTag(environment, tagName string) (*models.Tag, error) {
	tags, err := r.db.GetTagsByEnvironment(environment)
	if err != nil {
		return nil, err
	}

	for _, tag := range tags {
		if tag.Name == tagName {
			return convertDBTagToModel(*tag, environment), nil
		}
	}

	return nil, fmt.Errorf("tag '%s' not found in environment '%s'", tagName, environment)
}

// GetAvailableTags returns all available tags in an environment
func (r *Repository) GetAvailableTags(environment string) ([]*models.Tag, error) {
	tags, err := r.db.GetTagsByEnvironment(environment)
	if err != nil {
		return nil, err
	}

	var availableTags []*models.Tag
	for _, tag := range tags {
		if tag.Status == "available" {
			modelTag := convertDBTagToModel(*tag, environment)
			availableTags = append(availableTags, modelTag)
		}
	}

	return availableTags, nil
}

// Queue operations

// JoinQueue adds a user to the queue
func (r *Repository) JoinQueue(userID, username, environment, tag string, durationMinutes int) error {
	return r.db.JoinQueue(userID, username, environment, tag, durationMinutes)
}

// LeaveQueue removes a user from the queue
func (r *Repository) LeaveQueue(userID string) error {
	return r.db.LeaveQueue(userID)
}

// GetQueue returns all queue items
func (r *Repository) GetQueue() ([]models.QueueItem, error) {
	dbQueue, err := r.db.GetQueue()
	if err != nil {
		return nil, err
	}

	var queue []models.QueueItem
	for _, item := range dbQueue {
		queueItem := models.QueueItem{
			UserID:      item.UserID,
			Username:    item.Username,
			JoinedAt:    item.JoinedAt,
			Environment: item.Environment,
			Tag:         item.Tag,
			Duration:    time.Duration(item.DurationMinutes) * time.Minute,
		}
		queue = append(queue, queueItem)
	}

	return queue, nil
}

// GetUserPosition returns a user's position in the queue
func (r *Repository) GetUserPosition(userID string) (int, *models.QueueItem, error) {
	position, dbItem, err := r.db.GetUserQueuePosition(userID)
	if err != nil {
		return 0, nil, err
	}

	if dbItem == nil {
		return position, nil, nil
	}

	queueItem := &models.QueueItem{
		UserID:      dbItem.UserID,
		Username:    dbItem.Username,
		JoinedAt:    dbItem.JoinedAt,
		Environment: dbItem.Environment,
		Tag:         dbItem.Tag,
		Duration:    time.Duration(dbItem.DurationMinutes) * time.Minute,
	}

	return position, queueItem, nil
}

// IsQueueEmpty checks if the queue is empty for a specific environment/tag
func (r *Repository) IsQueueEmpty(environment, tag string) (bool, error) {
	return r.db.IsQueueEmptyForTag(environment, tag)
}

// ClearQueue removes all users from the queue
func (r *Repository) ClearQueue() (int, error) {
	return r.db.ClearQueue()
}

// ReleaseAllTags releases all currently assigned tags
func (r *Repository) ReleaseAllTags() (int, []string, error) {
	count, dbTags, err := r.db.ReleaseAllTags()
	if err != nil {
		return 0, nil, err
	}

	var tagNames []string
	for _, tag := range dbTags {
		tagNames = append(tagNames, fmt.Sprintf("%s/%s", tag.Environment, tag.Name))
	}

	return count, tagNames, nil
}

// TTL operations

// GetExpiredEvents returns expired tag assignments
func (r *Repository) GetExpiredEvents() ([]models.Tag, error) {
	dbTags, err := r.db.GetExpiredTags()
	if err != nil {
		return nil, err
	}

	var tags []models.Tag
	for _, tag := range dbTags {
		modelTag := convertDBTagToModel(tag, tag.Environment)
		tags = append(tags, *modelTag)
	}

	return tags, nil
}

// ProcessExpirationEvent processes a single expiration event
func (r *Repository) ProcessExpirationEvent(tagID int) error {
	return r.db.ProcessExpirationEvent(tagID)
}

// GetUpcomingExpirations returns tags that will expire within the specified window
func (r *Repository) GetUpcomingExpirations(window time.Duration) ([]models.Tag, error) {
	dbEvents, err := r.db.GetUpcomingExpirations(window)
	if err != nil {
		return nil, err
	}

	var tags []models.Tag
	for _, event := range dbEvents {
		// Convert ExpirationEvent to Tag - need to get tag details
		envTags, err := r.db.GetTagsByEnvironment(event.Environment)
		if err != nil {
			continue
		}

		for _, tag := range envTags {
			if tag.Name == event.TagName && tag.Status == "occupied" {
				modelTag := convertDBTagToModel(*tag, event.Environment)
				tags = append(tags, *modelTag)
				break
			}
		}
	}

	return tags, nil
}

// CleanupProcessedEvents removes old processed expiration events
func (r *Repository) CleanupProcessedEvents(cutoff time.Duration) error {
	return r.db.CleanupProcessedEvents(cutoff)
}

// TriggerDatabaseAutoExpiration manually triggers database-driven expiration checks
func (r *Repository) TriggerDatabaseAutoExpiration() (int, error) {
	return r.db.TriggerDatabaseAutoExpiration()
}

// GetAutoExpirationLogs returns recent auto-expiration log entries
func (r *Repository) GetAutoExpirationLogs(limit int) ([]AutoExpirationLog, error) {
	return r.db.GetAutoExpirationLogs(limit)
}

// GetExpiredTagsFromView uses the database view to get currently expired tags
func (r *Repository) GetExpiredTagsFromView() ([]models.Tag, error) {
	dbTags, err := r.db.GetExpiredTagsFromView()
	if err != nil {
		return nil, err
	}

	var tags []models.Tag
	for _, tag := range dbTags {
		modelTag := convertDBTagToModel(tag, tag.Environment)
		tags = append(tags, *modelTag)
	}

	return tags, nil
}

// CleanupAutoExpirationLogs removes old auto-expiration log entries
func (r *Repository) CleanupAutoExpirationLogs(olderThan time.Duration) error {
	return r.db.CleanupAutoExpirationLogs(olderThan)
}

// StartExpirationListener starts monitoring for database-driven expiration events
func (r *Repository) StartExpirationListener(notificationChan chan<- interface{}, dbPath string) error {
	// Convert the generic interface channel to the specific type
	typedChan := make(chan ExpirationNotification, 100)

	// Start a goroutine to bridge the channels
	go func() {
		for notification := range typedChan {
			notificationChan <- notification
		}
	}()

	return r.db.StartExpirationListener(typedChan, dbPath)
}

// TriggerDatabaseAutoExpirationWithNotifications triggers expiration and sends notifications
func (r *Repository) TriggerDatabaseAutoExpirationWithNotifications(notificationChan chan<- interface{}) (int, error) {
	// Convert the generic interface channel to the specific type
	typedChan := make(chan ExpirationNotification, 100)

	// Start a goroutine to bridge the channels
	go func() {
		for notification := range typedChan {
			notificationChan <- notification
		}
	}()

	return r.db.TriggerDatabaseAutoExpirationWithNotifications(typedChan)
}

// Utility

// Close closes the database connection
func (r *Repository) Close() error {
	return r.db.Close()
}

// GetTagsByEnvironment returns all tags for a specific environment as model pointers
func (r *Repository) GetTagsByEnvironment(envName string) ([]*models.Tag, error) {
	dbTags, err := r.db.GetTagsByEnvironment(envName)
	if err != nil {
		return nil, err
	}
	var tags []*models.Tag
	for _, dbTag := range dbTags {
		tags = append(tags, convertDBTagToModel(*dbTag, envName))
	}
	return tags, nil
}
