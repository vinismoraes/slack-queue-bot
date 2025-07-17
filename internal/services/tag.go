package services

import (
	"fmt"
	"log"
	"time"

	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
)

// TagService handles tag management operations
type TagService struct {
	db           interfaces.DatabaseRepository
	validation   interfaces.ValidationService
	notification interfaces.NotificationService
	config       interfaces.ConfigService
}

// NewTagService creates a new tag service
func NewTagService(
	db interfaces.DatabaseRepository,
	validation interfaces.ValidationService,
	notification interfaces.NotificationService,
	config interfaces.ConfigService,
) interfaces.TagService {
	return &TagService{
		db:           db,
		validation:   validation,
		notification: notification,
		config:       config,
	}
}

// AssignTag assigns a tag to a user for a specified duration
func (ts *TagService) AssignTag(userID string, environment, tag string, duration time.Duration) (*models.Tag, error) {
	log.Printf("TagService.AssignTag: userID=%s, env=%s, tag=%s, duration=%v", userID, environment, tag, duration)

	// Get the tag to verify it exists and is available
	tagObj, err := ts.db.GetTag(environment, tag)
	if err != nil {
		return nil, fmt.Errorf("failed to get tag: %w", err)
	}

	if tagObj.Status != "available" {
		return nil, fmt.Errorf("tag %s/%s is not available (status: %s)", environment, tag, tagObj.Status)
	}

	// Calculate expiration time
	expiresAt := time.Now().Add(duration)

	// Update tag status in database
	err = ts.db.UpdateTagStatus(environment, tag, "occupied", &userID, &expiresAt)
	if err != nil {
		return nil, fmt.Errorf("failed to update tag status: %w", err)
	}

	// Return updated tag information
	return &models.Tag{
		Name:        tag,
		Status:      "occupied",
		AssignedTo:  userID,
		AssignedAt:  time.Now(),
		ExpiresAt:   expiresAt,
		Environment: environment,
	}, nil
}

// ReleaseTag releases a tag from a user
func (ts *TagService) ReleaseTag(userID, environment, tag string) error {
	log.Printf("TagService.ReleaseTag: userID=%s, env=%s, tag=%s", userID, environment, tag)

	// Get the tag to verify the user has it assigned
	tagObj, err := ts.db.GetTag(environment, tag)
	if err != nil {
		return fmt.Errorf("failed to get tag: %w", err)
	}

	if tagObj.Status != "occupied" || tagObj.AssignedTo != userID {
		return fmt.Errorf("you are not assigned to tag %s/%s", environment, tag)
	}

	// Release the tag
	err = ts.db.UpdateTagStatus(environment, tag, "available", nil, nil)
	if err != nil {
		return fmt.Errorf("failed to release tag: %w", err)
	}

	// Notify about release
	ts.notification.NotifyTagRelease(userID, tag, environment)
	ts.notification.BroadcastQueueUpdate()

	log.Printf("Tag %s/%s released by user %s", environment, tag, userID)
	return nil
}

// ExtendAssignment extends a user's assignment by the specified duration
func (ts *TagService) ExtendAssignment(userID, environment, tag string, extension time.Duration) error {
	log.Printf("TagService.ExtendAssignment: userID=%s, env=%s, tag=%s, extension=%v", userID, environment, tag, extension)

	// Get the tag to verify the user has it assigned
	tagObj, err := ts.db.GetTag(environment, tag)
	if err != nil {
		return fmt.Errorf("failed to get tag: %w", err)
	}

	if tagObj.Status != "occupied" || tagObj.AssignedTo != userID {
		return fmt.Errorf("you are not assigned to tag %s/%s", environment, tag)
	}

	// Calculate new expiration time
	newExpiresAt := tagObj.ExpiresAt.Add(extension)

	// Update the expiration time
	err = ts.db.UpdateTagStatus(environment, tag, "occupied", &userID, &newExpiresAt)
	if err != nil {
		return fmt.Errorf("failed to extend assignment: %w", err)
	}

	// Get updated tag for notification
	updatedTag, _ := ts.db.GetTag(environment, tag)
	ts.notification.NotifyAssignmentExtended(userID, updatedTag, environment, extension)
	ts.notification.BroadcastQueueUpdate()

	log.Printf("Tag %s/%s assignment extended for user %s until %s", environment, tag, userID, newExpiresAt.Format("15:04"))
	return nil
}

// GetAvailableTag returns the first available tag in an environment
func (ts *TagService) GetAvailableTag(environment string) (*models.Tag, error) {
	log.Printf("TagService.GetAvailableTag: env=%s", environment)

	availableTags, err := ts.db.GetAvailableTags(environment)
	if err != nil {
		return nil, fmt.Errorf("failed to get available tags: %w", err)
	}

	if len(availableTags) == 0 {
		return nil, fmt.Errorf("no available tags in environment %s", environment)
	}

	// Return the first available tag
	return availableTags[0], nil
}

// GetUserAssignment returns the tag assigned to a user
func (ts *TagService) GetUserAssignment(userID string) (*models.Tag, error) {
	log.Printf("TagService.GetUserAssignment: userID=%s", userID)

	// Get all environments to search for user assignments
	environments, err := ts.db.GetEnvironments()
	if err != nil {
		return nil, fmt.Errorf("failed to get environments: %w", err)
	}

	// Search through all environments and tags
	for _, env := range environments {
		for _, tag := range env.Tags {
			if tag.Status == "occupied" && tag.AssignedTo == userID {
				return tag, nil
			}
		}
	}

	return nil, fmt.Errorf("user %s has no active assignment", userID)
}

// UpdateTagStatus updates the status of a tag
func (ts *TagService) UpdateTagStatus(environment, tag, status string) error {
	log.Printf("TagService.UpdateTagStatus: env=%s, tag=%s, status=%s", environment, tag, status)

	// Validate status
	if status != "available" && status != "occupied" && status != "maintenance" {
		return fmt.Errorf("invalid status: %s (must be available, occupied, or maintenance)", status)
	}

	// For maintenance status, clear assignment info
	if status == "maintenance" {
		err := ts.db.UpdateTagStatus(environment, tag, status, nil, nil)
		if err != nil {
			return fmt.Errorf("failed to update tag status: %w", err)
		}
	} else if status == "available" {
		err := ts.db.UpdateTagStatus(environment, tag, status, nil, nil)
		if err != nil {
			return fmt.Errorf("failed to update tag status: %w", err)
		}
	} else {
		// For occupied status, additional info should be provided via AssignTag
		return fmt.Errorf("use AssignTag method to set occupied status")
	}

	ts.notification.BroadcastQueueUpdate()

	log.Printf("Tag %s/%s status updated to %s", environment, tag, status)
	return nil
}

// GetExpiredTags returns all tags that have expired
func (ts *TagService) GetExpiredTags() []*models.Tag {
	expiredEvents, err := ts.db.GetExpiredEvents()
	if err != nil {
		log.Printf("Error getting expired events: %v", err)
		return []*models.Tag{}
	}

	// Convert to Tag pointers
	var expiredTags []*models.Tag
	for _, event := range expiredEvents {
		expiredTags = append(expiredTags, &event)
	}

	return expiredTags
}

// ReleaseExpiredTags releases all expired tags
func (ts *TagService) ReleaseExpiredTags() error {
	expiredEvents, err := ts.db.GetExpiredEvents()
	if err != nil {
		return fmt.Errorf("failed to get expired events: %w", err)
	}

	for _, event := range expiredEvents {
		// Release the tag
		err := ts.db.UpdateTagStatus(event.Environment, event.Name, "available", nil, nil)
		if err != nil {
			log.Printf("Error releasing expired tag %s/%s: %v", event.Environment, event.Name, err)
			continue
		}

		// Notify user about expiration
		ts.notification.NotifyExpiration(event.AssignedTo, &event, event.Environment)

		log.Printf("Released expired tag %s/%s (was assigned to %s)", event.Environment, event.Name, event.AssignedTo)
	}

	if len(expiredEvents) > 0 {
		ts.notification.BroadcastQueueUpdate()
	}

	return nil
}

// GetUpcomingExpirations returns tags that will expire within the specified duration
func (ts *TagService) GetUpcomingExpirations(within time.Duration) []*models.Tag {
	environments, err := ts.db.GetEnvironments()
	if err != nil {
		log.Printf("Error getting environments for upcoming expirations: %v", err)
		return []*models.Tag{}
	}

	var upcoming []*models.Tag
	futureTime := time.Now().Add(within)

	for _, env := range environments {
		for _, tag := range env.Tags {
			if tag.Status == "occupied" && !tag.ExpiresAt.IsZero() && tag.ExpiresAt.Before(futureTime) && tag.ExpiresAt.After(time.Now()) {
				upcoming = append(upcoming, tag)
			}
		}
	}

	return upcoming
}
