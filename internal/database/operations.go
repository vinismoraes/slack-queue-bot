package database

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// Environment Operations

// CreateEnvironment creates a new environment with its tags
func (db *DB) CreateEnvironment(name string, tags []string) error {
	return db.CreateEnvironmentWithDisplayName(name, "", tags)
}

// CreateEnvironmentWithDisplayName creates a new environment with its tags and optional display name
func (db *DB) CreateEnvironmentWithDisplayName(name, displayName string, tags []string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// If no display name provided, generate one from name
	if displayName == "" {
		displayName = generateDisplayName(name)
	}

	// Insert environment
	result, err := tx.Exec("INSERT INTO environments (name, display_name) VALUES (?, ?)", name, displayName)
	if err != nil {
		return fmt.Errorf("failed to insert environment: %w", err)
	}

	envID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get environment ID: %w", err)
	}

	// Insert tags
	for _, tagName := range tags {
		_, err := tx.Exec("INSERT INTO tags (name, environment_id) VALUES (?, ?)", tagName, envID)
		if err != nil {
			return fmt.Errorf("failed to insert tag %s: %w", tagName, err)
		}
	}

	return tx.Commit()
}

// generateDisplayName creates a display name from an environment name
func generateDisplayName(name string) string {
	// Replace dashes with spaces and title case
	displayName := strings.ReplaceAll(name, "-", " ")
	words := strings.Fields(displayName)
	for i, word := range words {
		words[i] = strings.Title(strings.ToLower(word))
	}
	return strings.Join(words, " ")
}

// GetEnvironments returns all environments with their tags
func (db *DB) GetEnvironments() ([]Environment, error) {
	// Get environments
	rows, err := db.conn.Query("SELECT id, name, display_name, created_at FROM environments ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("failed to query environments: %w", err)
	}
	defer rows.Close()

	var environments []Environment
	for rows.Next() {
		var env Environment
		var displayName sql.NullString
		if err := rows.Scan(&env.ID, &env.Name, &displayName, &env.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan environment: %w", err)
		}

		// Handle nullable display_name
		if displayName.Valid {
			env.DisplayName = displayName.String
		} else {
			env.DisplayName = generateDisplayName(env.Name)
		}

		// Get tags for this environment
		tags, err := db.getTagsForEnvironment(env.ID, env.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get tags for environment %s: %w", env.Name, err)
		}
		env.Tags = tags

		environments = append(environments, env)
	}

	return environments, nil
}

// getTagsForEnvironment gets all tags for a specific environment
func (db *DB) getTagsForEnvironment(envID int, envName string) ([]Tag, error) {
	rows, err := db.conn.Query(`
		SELECT id, name, environment_id, status, assigned_to, assigned_at, expires_at, created_at
		FROM tags
		WHERE environment_id = ?
		ORDER BY name`, envID)
	if err != nil {
		return nil, fmt.Errorf("failed to query tags: %w", err)
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.EnvironmentID, &tag.Status,
			&tag.AssignedTo, &tag.AssignedAt, &tag.ExpiresAt, &tag.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan tag: %w", err)
		}
		tag.Environment = envName
		tags = append(tags, tag)
	}

	return tags, nil
}

// GetTagsByEnvironment gets all tags for a specific environment by name
func (db *DB) GetTagsByEnvironment(envName string) ([]*Tag, error) {
	// First get the environment ID
	var envID int
	err := db.conn.QueryRow("SELECT id FROM environments WHERE name = ?", envName).Scan(&envID)
	if err != nil {
		return nil, fmt.Errorf("environment not found: %w", err)
	}

	rows, err := db.conn.Query(`
		SELECT id, name, environment_id, status, assigned_to, assigned_at, expires_at, created_at
		FROM tags
		WHERE environment_id = ?
		ORDER BY name`, envID)
	if err != nil {
		return nil, fmt.Errorf("failed to query tags: %w", err)
	}
	defer rows.Close()

	var tags []*Tag
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.EnvironmentID, &tag.Status,
			&tag.AssignedTo, &tag.AssignedAt, &tag.ExpiresAt, &tag.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan tag: %w", err)
		}
		tag.Environment = envName
		tags = append(tags, &tag)
	}

	return tags, nil
}

// Queue Operations

// JoinQueue adds a user to the queue atomically
func (db *DB) JoinQueue(userID, username, environment, tag string, durationMinutes int) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Check if user is already in queue for this specific environment+tag
	var count int
	err = tx.QueryRow("SELECT COUNT(*) FROM queue WHERE user_id = ? AND environment = ? AND tag = ?", userID, environment, tag).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if user in queue: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("user is already in queue for %s/%s", environment, tag)
	}

	// Get next position in queue
	var maxPosition int
	err = tx.QueryRow("SELECT COALESCE(MAX(position), 0) FROM queue").Scan(&maxPosition)
	if err != nil {
		return fmt.Errorf("failed to get max position: %w", err)
	}

	// Insert into queue
	_, err = tx.Exec(`
		INSERT INTO queue (user_id, username, environment, tag, duration_minutes, position)
		VALUES (?, ?, ?, ?, ?, ?)`,
		userID, username, environment, tag, durationMinutes, maxPosition+1)
	if err != nil {
		return fmt.Errorf("failed to insert into queue: %w", err)
	}

	return tx.Commit()
}

// LeaveQueue removes a user from the queue atomically
func (db *DB) LeaveQueue(userID string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get user's position
	var position int
	err = tx.QueryRow("SELECT position FROM queue WHERE user_id = ?", userID).Scan(&position)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("user not in queue")
		}
		return fmt.Errorf("failed to get user position: %w", err)
	}

	// Remove user from queue
	_, err = tx.Exec("DELETE FROM queue WHERE user_id = ?", userID)
	if err != nil {
		return fmt.Errorf("failed to delete from queue: %w", err)
	}

	// Update positions of users who were after this user
	_, err = tx.Exec("UPDATE queue SET position = position - 1 WHERE position > ?", position)
	if err != nil {
		return fmt.Errorf("failed to update positions: %w", err)
	}

	return tx.Commit()
}

// ClearQueue removes all users from the queue atomically
func (db *DB) ClearQueue() (int, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Count current queue size
	var count int
	err = tx.QueryRow("SELECT COUNT(*) FROM queue").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count queue: %w", err)
	}

	if count == 0 {
		return 0, nil // Nothing to clear
	}

	// Clear all queue entries
	_, err = tx.Exec("DELETE FROM queue")
	if err != nil {
		return 0, fmt.Errorf("failed to clear queue: %w", err)
	}

	return count, tx.Commit()
}

// GetQueue returns all users in the queue ordered by position
func (db *DB) GetQueue() ([]QueueItem, error) {
	rows, err := db.conn.Query(`
		SELECT id, user_id, username, environment, tag, duration_minutes, joined_at, position
		FROM queue
		ORDER BY position`)
	if err != nil {
		return nil, fmt.Errorf("failed to query queue: %w", err)
	}
	defer rows.Close()

	var queue []QueueItem
	for rows.Next() {
		var item QueueItem
		if err := rows.Scan(&item.ID, &item.UserID, &item.Username, &item.Environment,
			&item.Tag, &item.DurationMinutes, &item.JoinedAt, &item.Position); err != nil {
			return nil, fmt.Errorf("failed to scan queue item: %w", err)
		}
		queue = append(queue, item)
	}

	return queue, nil
}

// GetUserQueuePosition returns a user's position in the queue (1-based)
func (db *DB) GetUserQueuePosition(userID string) (int, *QueueItem, error) {
	var item QueueItem
	err := db.conn.QueryRow(`
		SELECT id, user_id, username, environment, tag, duration_minutes, joined_at, position
		FROM queue
		WHERE user_id = ?`, userID).Scan(
		&item.ID, &item.UserID, &item.Username, &item.Environment,
		&item.Tag, &item.DurationMinutes, &item.JoinedAt, &item.Position)

	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil, nil // User not in queue
		}
		return 0, nil, fmt.Errorf("failed to get user queue position: %w", err)
	}

	return item.Position, &item, nil
}

// IsQueueEmptyForTag checks if there are any users in queue for a specific tag
func (db *DB) IsQueueEmptyForTag(environment, tag string) (bool, error) {
	var count int
	err := db.conn.QueryRow("SELECT COUNT(*) FROM queue WHERE environment = ? AND tag = ?", environment, tag).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check queue for tag: %w", err)
	}
	return count == 0, nil
}

// Tag Operations

// UpdateTagStatus updates a tag's status and assignment info atomically
func (db *DB) UpdateTagStatus(environment, tagName, status string, assignedTo *string, expiresAt *time.Time) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var assignedAt *time.Time
	if assignedTo != nil {
		now := time.Now().UTC()
		assignedAt = &now
	}

	_, err = tx.Exec(`
		UPDATE tags
		SET status = ?, assigned_to = ?, assigned_at = ?, expires_at = ?
		WHERE name = ? AND environment_id = (SELECT id FROM environments WHERE name = ?)`,
		status, assignedTo, assignedAt, expiresAt, tagName, environment)
	if err != nil {
		return fmt.Errorf("failed to update tag status: %w", err)
	}

	return tx.Commit()
}

// GetAvailableTags returns all available tags across all environments
func (db *DB) GetAvailableTags() ([]Tag, error) {
	rows, err := db.conn.Query(`
		SELECT t.id, t.name, t.environment_id, e.name as environment, t.status,
		       t.assigned_to, t.assigned_at, t.expires_at, t.created_at
		FROM tags t
		JOIN environments e ON t.environment_id = e.id
		WHERE t.status = 'available'
		ORDER BY e.name, t.name`)
	if err != nil {
		return nil, fmt.Errorf("failed to query available tags: %w", err)
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.EnvironmentID, &tag.Environment,
			&tag.Status, &tag.AssignedTo, &tag.AssignedAt, &tag.ExpiresAt, &tag.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan tag: %w", err)
		}
		tags = append(tags, tag)
	}

	return tags, nil
}

// GetExpiredTags returns all tags that have expired
func (db *DB) GetExpiredTags() ([]Tag, error) {
	rows, err := db.conn.Query(`
		SELECT t.id, t.name, t.environment_id, e.name as environment, t.status,
		       t.assigned_to, t.assigned_at, t.expires_at, t.created_at
		FROM tags t
		JOIN environments e ON t.environment_id = e.id
		WHERE t.status = 'occupied' AND t.expires_at < datetime('now')
		ORDER BY t.expires_at`)
	if err != nil {
		return nil, fmt.Errorf("failed to query expired tags: %w", err)
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.EnvironmentID, &tag.Environment,
			&tag.Status, &tag.AssignedTo, &tag.AssignedAt, &tag.ExpiresAt, &tag.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan tag: %w", err)
		}
		tags = append(tags, tag)
	}

	return tags, nil
}

// GetOccupiedTags returns all currently assigned/occupied tags
func (db *DB) GetOccupiedTags() ([]Tag, error) {
	rows, err := db.conn.Query(`
		SELECT t.id, t.name, t.environment_id, e.name as environment, t.status,
		       t.assigned_to, t.assigned_at, t.expires_at, t.created_at
		FROM tags t
		JOIN environments e ON t.environment_id = e.id
		WHERE t.status = 'occupied'
		ORDER BY e.name, t.name`)
	if err != nil {
		return nil, fmt.Errorf("failed to query occupied tags: %w", err)
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.EnvironmentID, &tag.Environment,
			&tag.Status, &tag.AssignedTo, &tag.AssignedAt, &tag.ExpiresAt, &tag.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan tag: %w", err)
		}
		tags = append(tags, tag)
	}

	return tags, nil
}

// ReleaseAllTags releases all currently assigned tags atomically
func (db *DB) ReleaseAllTags() (int, []Tag, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get all occupied tags first for the return value
	rows, err := tx.Query(`
		SELECT t.id, t.name, t.environment_id, e.name as environment, t.status,
		       t.assigned_to, t.assigned_at, t.expires_at, t.created_at
		FROM tags t
		JOIN environments e ON t.environment_id = e.id
		WHERE t.status = 'occupied'`)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to query occupied tags: %w", err)
	}

	var releasedTags []Tag
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.EnvironmentID, &tag.Environment,
			&tag.Status, &tag.AssignedTo, &tag.AssignedAt, &tag.ExpiresAt, &tag.CreatedAt); err != nil {
			return 0, nil, fmt.Errorf("failed to scan tag: %w", err)
		}
		releasedTags = append(releasedTags, tag)
	}
	rows.Close()

	if len(releasedTags) == 0 {
		return 0, nil, nil // Nothing to release
	}

	// Release all occupied tags
	_, err = tx.Exec(`
		UPDATE tags
		SET status = 'available', assigned_to = NULL, assigned_at = NULL, expires_at = NULL
		WHERE status = 'occupied'`)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to release all tags: %w", err)
	}

	// Mark all related expiration events as processed
	_, err = tx.Exec(`
		UPDATE expiration_events
		SET processed = TRUE
		WHERE processed = FALSE`)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to mark expiration events as processed: %w", err)
	}

	return len(releasedTags), releasedTags, tx.Commit()
}

// HasUserActiveAssignment checks if user has any active tag assignments
func (db *DB) HasUserActiveAssignment(userID string) (bool, *Tag, error) {
	var tag Tag
	err := db.conn.QueryRow(`
		SELECT t.id, t.name, t.environment_id, e.name as environment, t.status,
		       t.assigned_to, t.assigned_at, t.expires_at, t.created_at
		FROM tags t
		JOIN environments e ON t.environment_id = e.id
		WHERE t.status = 'occupied' AND t.assigned_to = ?
		LIMIT 1`, userID).Scan(
		&tag.ID, &tag.Name, &tag.EnvironmentID, &tag.Environment,
		&tag.Status, &tag.AssignedTo, &tag.AssignedAt, &tag.ExpiresAt, &tag.CreatedAt)

	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("failed to check user assignment: %w", err)
	}

	return true, &tag, nil
}

// TTL-based Expiration Operations

// ExpirationEvent represents an automatic expiration event
type ExpirationEvent struct {
	ID          int       `json:"id"`
	TagID       int       `json:"tag_id"`
	Environment string    `json:"environment"`
	TagName     string    `json:"tag_name"`
	AssignedTo  string    `json:"assigned_to"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
	Processed   bool      `json:"processed"`
}

// GetExpiredEvents returns all unprocessed expiration events that have expired
func (db *DB) GetExpiredEvents() ([]ExpirationEvent, error) {
	currentTime := time.Now()
	rows, err := db.conn.Query(`
		SELECT id, tag_id, environment, tag_name, assigned_to, expires_at, created_at, processed
		FROM expiration_events
		WHERE processed = FALSE AND expires_at <= ?
		ORDER BY expires_at`, currentTime)
	if err != nil {
		return nil, fmt.Errorf("failed to query expired events: %w", err)
	}
	defer rows.Close()

	var events []ExpirationEvent
	for rows.Next() {
		var event ExpirationEvent
		if err := rows.Scan(&event.ID, &event.TagID, &event.Environment, &event.TagName,
			&event.AssignedTo, &event.ExpiresAt, &event.CreatedAt, &event.Processed); err != nil {
			return nil, fmt.Errorf("failed to scan expiration event: %w", err)
		}
		events = append(events, event)
	}

	return events, nil
}

// ProcessExpirationEvent handles a single expiration event by releasing the tag and marking the event as processed
func (db *DB) ProcessExpirationEvent(eventID int) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get the event details
	var event ExpirationEvent
	err = tx.QueryRow(`
		SELECT id, tag_id, environment, tag_name, assigned_to, expires_at
		FROM expiration_events
		WHERE id = ? AND processed = FALSE`, eventID).Scan(
		&event.ID, &event.TagID, &event.Environment, &event.TagName,
		&event.AssignedTo, &event.ExpiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil // Event already processed or doesn't exist
		}
		return fmt.Errorf("failed to get expiration event: %w", err)
	}

	// Release the tag (this will trigger the release trigger to mark event as processed)
	_, err = tx.Exec(`
		UPDATE tags
		SET status = 'available', assigned_to = NULL, assigned_at = NULL, expires_at = NULL
		WHERE id = ? AND status = 'occupied'`, event.TagID)
	if err != nil {
		return fmt.Errorf("failed to release expired tag: %w", err)
	}

	// Mark the event as processed (in case the trigger didn't catch it)
	_, err = tx.Exec(`
		UPDATE expiration_events
		SET processed = TRUE
		WHERE id = ?`, eventID)
	if err != nil {
		return fmt.Errorf("failed to mark event as processed: %w", err)
	}

	return tx.Commit()
}

// GetUpcomingExpirations returns expiration events that will expire within the specified duration
func (db *DB) GetUpcomingExpirations(within time.Duration) ([]ExpirationEvent, error) {
	currentTime := time.Now()
	expiryThreshold := currentTime.Add(within)

	rows, err := db.conn.Query(`
		SELECT id, tag_id, environment, tag_name, assigned_to, expires_at, created_at, processed
		FROM expiration_events
		WHERE processed = FALSE
		AND expires_at > ?
		AND expires_at <= ?
		ORDER BY expires_at`, currentTime, expiryThreshold)
	if err != nil {
		return nil, fmt.Errorf("failed to query upcoming expirations: %w", err)
	}
	defer rows.Close()

	var events []ExpirationEvent
	for rows.Next() {
		var event ExpirationEvent
		if err := rows.Scan(&event.ID, &event.TagID, &event.Environment, &event.TagName,
			&event.AssignedTo, &event.ExpiresAt, &event.CreatedAt, &event.Processed); err != nil {
			return nil, fmt.Errorf("failed to scan upcoming expiration event: %w", err)
		}
		events = append(events, event)
	}

	return events, nil
}

// CleanupProcessedEvents removes old processed expiration events (housekeeping)
func (db *DB) CleanupProcessedEvents(olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan)

	_, err := db.conn.Exec(`
		DELETE FROM expiration_events
		WHERE processed = TRUE AND created_at < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("failed to cleanup processed events: %w", err)
	}

	return nil
}

// TriggerDatabaseAutoExpiration manually triggers the database auto-expiration system
// This forces the database to check and release any expired tags immediately
func (db *DB) TriggerDatabaseAutoExpiration() (int, error) {
	// Count expired tags before processing
	var expiredCount int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM tags
		WHERE status = 'occupied'
			AND expires_at IS NOT NULL
			AND expires_at < datetime('now')`).Scan(&expiredCount)
	if err != nil {
		return 0, fmt.Errorf("failed to count expired tags: %w", err)
	}

	if expiredCount == 0 {
		return 0, nil // No expired tags
	}

	// Log the expired tags before releasing them
	_, err = db.conn.Exec(`
		INSERT INTO auto_expiration_log (tag_id, environment, tag_name, assigned_to, processed_by)
		SELECT t.id, e.name, t.name, t.assigned_to, 'manual_trigger'
		FROM tags t
		JOIN environments e ON t.environment_id = e.id
		WHERE t.status = 'occupied'
			AND t.expires_at IS NOT NULL
			AND t.expires_at < datetime('now')`)
	if err != nil {
		return 0, fmt.Errorf("failed to log auto-expirations: %w", err)
	}

	// Release expired tags
	_, err = db.conn.Exec(`
		UPDATE tags
		SET status = 'available', assigned_to = NULL, assigned_at = NULL, expires_at = NULL
		WHERE status = 'occupied'
			AND expires_at IS NOT NULL
			AND expires_at < datetime('now')`)
	if err != nil {
		return 0, fmt.Errorf("failed to release expired tags: %w", err)
	}

	// Mark expiration events as processed
	_, err = db.conn.Exec(`
		UPDATE expiration_events
		SET processed = TRUE
		WHERE expires_at < datetime('now') AND processed = FALSE`)
	if err != nil {
		return 0, fmt.Errorf("failed to mark expiration events as processed: %w", err)
	}

	return expiredCount, nil
}

// AutoExpirationLog represents a log entry of database-driven auto-expiration
type AutoExpirationLog struct {
	ID          int       `json:"id"`
	TagID       int       `json:"tag_id"`
	Environment string    `json:"environment"`
	TagName     string    `json:"tag_name"`
	AssignedTo  string    `json:"assigned_to"`
	ExpiredAt   time.Time `json:"expired_at"`
	ProcessedBy string    `json:"processed_by"`
}

// GetAutoExpirationLogs returns recent auto-expiration log entries
func (db *DB) GetAutoExpirationLogs(limit int) ([]AutoExpirationLog, error) {
	if limit <= 0 {
		limit = 50 // Default limit
	}

	rows, err := db.conn.Query(`
		SELECT id, tag_id, environment, tag_name, assigned_to, expired_at, processed_by
		FROM auto_expiration_log
		ORDER BY expired_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query auto-expiration logs: %w", err)
	}
	defer rows.Close()

	var logs []AutoExpirationLog
	for rows.Next() {
		var log AutoExpirationLog
		if err := rows.Scan(&log.ID, &log.TagID, &log.Environment, &log.TagName,
			&log.AssignedTo, &log.ExpiredAt, &log.ProcessedBy); err != nil {
			return nil, fmt.Errorf("failed to scan auto-expiration log: %w", err)
		}
		logs = append(logs, log)
	}

	return logs, nil
}

// GetExpiredTagsFromView uses the database view to get currently expired tags
func (db *DB) GetExpiredTagsFromView() ([]Tag, error) {
	rows, err := db.conn.Query(`
		SELECT id, name, environment, assigned_to, expires_at
		FROM expired_tags_view
		ORDER BY minutes_overdue DESC`)
	if err != nil {
		return nil, fmt.Errorf("failed to query expired tags view: %w", err)
	}
	defer rows.Close()

	var tags []Tag
	for rows.Next() {
		var tag Tag
		var expiresAt *time.Time
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.Environment, &tag.AssignedTo, &expiresAt); err != nil {
			return nil, fmt.Errorf("failed to scan expired tag: %w", err)
		}
		if expiresAt != nil {
			tag.ExpiresAt = expiresAt
		}
		tag.Status = "occupied" // These are all occupied but expired
		tags = append(tags, tag)
	}

	return tags, nil
}

// CleanupAutoExpirationLogs removes old auto-expiration log entries
func (db *DB) CleanupAutoExpirationLogs(olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan)

	_, err := db.conn.Exec(`
		DELETE FROM auto_expiration_log
		WHERE expired_at < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("failed to cleanup auto-expiration logs: %w", err)
	}

	return nil
}

// ExpirationEvent represents a tag expiration event for real-time notifications
type ExpirationNotification struct {
	TagID       int       `json:"tag_id"`
	Environment string    `json:"environment"`
	TagName     string    `json:"tag_name"`
	AssignedTo  string    `json:"assigned_to"`
	ExpiredAt   time.Time `json:"expired_at"`
	ProcessedBy string    `json:"processed_by"`
}

// StartExpirationListener starts a goroutine that monitors for expiration events
// and sends notifications through the provided channel
func (db *DB) StartExpirationListener(notificationChan chan<- ExpirationNotification, dbPath string) error {
	// Create a separate connection for listening to avoid blocking main operations
	listenerConn, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		return fmt.Errorf("failed to create listener connection: %w", err)
	}

	// Start monitoring goroutine
	go func() {
		defer listenerConn.Close()

		log.Println("Starting database expiration listener...")

		// Poll for new expiration events every few seconds
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		// Initialize lastCheckedID to the latest ID in the log to avoid processing old entries
		var lastCheckedID int
		err := listenerConn.QueryRow("SELECT COALESCE(MAX(id), 0) FROM auto_expiration_log").Scan(&lastCheckedID)
		if err != nil {
			log.Printf("Warning: Could not get latest expiration log ID, starting from 0: %v", err)
			lastCheckedID = 0
		} else {
			log.Printf("Starting expiration listener from log ID %d", lastCheckedID)
		}

		for {
			select {
			case <-ticker.C:
				// Query for new auto-expiration log entries
				rows, err := listenerConn.Query(`
					SELECT id, tag_id, environment, tag_name, assigned_to, expired_at, processed_by
					FROM auto_expiration_log
					WHERE id > ?
					ORDER BY id ASC`, lastCheckedID)
				if err != nil {
					log.Printf("Error querying expiration events: %v", err)
					continue
				}

				var newExpirations []ExpirationNotification
				var maxID int

				for rows.Next() {
					var id int
					var notification ExpirationNotification

					if err := rows.Scan(&id, &notification.TagID, &notification.Environment,
						&notification.TagName, &notification.AssignedTo,
						&notification.ExpiredAt, &notification.ProcessedBy); err != nil {
						log.Printf("Error scanning expiration event: %v", err)
						continue
					}

					newExpirations = append(newExpirations, notification)
					if id > maxID {
						maxID = id
					}
				}
				rows.Close()

				// Send notifications for new expirations
				for _, notification := range newExpirations {
					select {
					case notificationChan <- notification:
						log.Printf("Sent expiration notification for %s/%s (user: %s)",
							notification.Environment, notification.TagName, notification.AssignedTo)
					default:
						log.Printf("Warning: Notification channel full, skipping expiration notification")
					}
				}

				// Update the last checked ID
				if maxID > lastCheckedID {
					lastCheckedID = maxID
				}
			}
		}
	}()

	return nil
}

// TriggerDatabaseAutoExpirationWithNotifications is enhanced to send notifications
func (db *DB) TriggerDatabaseAutoExpirationWithNotifications(notificationChan chan<- ExpirationNotification) (int, error) {
	// Get expired tags first for notifications
	expiredTags, err := db.GetExpiredTagsFromView()
	if err != nil {
		return 0, fmt.Errorf("failed to get expired tags: %w", err)
	}

	if len(expiredTags) == 0 {
		return 0, nil // No expired tags
	}

	// Log the expired tags before releasing them
	_, err = db.conn.Exec(`
		INSERT INTO auto_expiration_log (tag_id, environment, tag_name, assigned_to, processed_by)
		SELECT t.id, e.name, t.name, t.assigned_to, 'manual_trigger_with_notification'
		FROM tags t
		JOIN environments e ON t.environment_id = e.id
		WHERE t.status = 'occupied'
			AND t.expires_at IS NOT NULL
			AND t.expires_at < datetime('now')`)
	if err != nil {
		return 0, fmt.Errorf("failed to log auto-expirations: %w", err)
	}

	// Send immediate notifications for expired tags
	for _, tag := range expiredTags {
		assignedTo := ""
		if tag.AssignedTo != nil {
			assignedTo = *tag.AssignedTo
		}

		expiresAt := time.Now()
		if tag.ExpiresAt != nil {
			expiresAt = *tag.ExpiresAt
		}

		notification := ExpirationNotification{
			TagID:       tag.ID,
			Environment: tag.Environment,
			TagName:     tag.Name,
			AssignedTo:  assignedTo,
			ExpiredAt:   expiresAt,
			ProcessedBy: "manual_trigger_immediate",
		}

		select {
		case notificationChan <- notification:
			log.Printf("Sent immediate expiration notification for %s/%s (user: %s)",
				notification.Environment, notification.TagName, notification.AssignedTo)
		default:
			log.Printf("Warning: Notification channel full, skipping immediate expiration notification")
		}
	}

	// Release expired tags
	_, err = db.conn.Exec(`
		UPDATE tags
		SET status = 'available', assigned_to = NULL, assigned_at = NULL, expires_at = NULL
		WHERE status = 'occupied'
			AND expires_at IS NOT NULL
			AND expires_at < datetime('now')`)
	if err != nil {
		return 0, fmt.Errorf("failed to release expired tags: %w", err)
	}

	// Mark expiration events as processed
	_, err = db.conn.Exec(`
		UPDATE expiration_events
		SET processed = TRUE
		WHERE expires_at < datetime('now') AND processed = FALSE`)
	if err != nil {
		return 0, fmt.Errorf("failed to mark expiration events as processed: %w", err)
	}

	return len(expiredTags), nil
}

// Configuration Operations

// ConfigSetting represents a configuration setting
type ConfigSetting struct {
	ID           int       `json:"id"`
	SettingKey   string    `json:"setting_key"`
	SettingValue string    `json:"setting_value"`
	SettingType  string    `json:"setting_type"`
	Description  string    `json:"description"`
	UpdatedAt    time.Time `json:"updated_at"`
	CreatedAt    time.Time `json:"created_at"`
}

// GetConfigSettings returns all configuration settings
func (db *DB) GetConfigSettings() (map[string]ConfigSetting, error) {
	rows, err := db.conn.Query(`
		SELECT id, setting_key, setting_value, setting_type, description, updated_at, created_at
		FROM config_settings
		ORDER BY setting_key`)
	if err != nil {
		return nil, fmt.Errorf("failed to query config settings: %w", err)
	}
	defer rows.Close()

	settings := make(map[string]ConfigSetting)
	for rows.Next() {
		var setting ConfigSetting
		if err := rows.Scan(&setting.ID, &setting.SettingKey, &setting.SettingValue,
			&setting.SettingType, &setting.Description, &setting.UpdatedAt, &setting.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan config setting: %w", err)
		}
		settings[setting.SettingKey] = setting
	}

	return settings, nil
}

// GetConfigSetting returns a specific configuration setting
func (db *DB) GetConfigSetting(key string) (*ConfigSetting, error) {
	var setting ConfigSetting
	err := db.conn.QueryRow(`
		SELECT id, setting_key, setting_value, setting_type, description, updated_at, created_at
		FROM config_settings
		WHERE setting_key = ?`, key).Scan(
		&setting.ID, &setting.SettingKey, &setting.SettingValue,
		&setting.SettingType, &setting.Description, &setting.UpdatedAt, &setting.CreatedAt)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("config setting '%s' not found", key)
		}
		return nil, fmt.Errorf("failed to get config setting: %w", err)
	}

	return &setting, nil
}

// SetConfigSetting updates or creates a configuration setting
func (db *DB) SetConfigSetting(key, value, settingType, description string) error {
	_, err := db.conn.Exec(`
		INSERT INTO config_settings (setting_key, setting_value, setting_type, description, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(setting_key) DO UPDATE SET
			setting_value = excluded.setting_value,
			setting_type = excluded.setting_type,
			description = excluded.description,
			updated_at = CURRENT_TIMESTAMP`, key, value, settingType, description)
	if err != nil {
		return fmt.Errorf("failed to set config setting: %w", err)
	}

	return nil
}

// DeleteConfigSetting removes a configuration setting
func (db *DB) DeleteConfigSetting(key string) error {
	_, err := db.conn.Exec("DELETE FROM config_settings WHERE setting_key = ?", key)
	if err != nil {
		return fmt.Errorf("failed to delete config setting: %w", err)
	}
	return nil
}

// MigrateConfigFromJSON migrates configuration from JSON to database
func (db *DB) MigrateConfigFromJSON(jsonPath string) error {
	// This will be implemented to read from the existing config.json and populate the database
	// Default settings are inserted during schema initialization
	log.Printf("Configuration migration would read from %s and populate database", jsonPath)
	return nil
}
