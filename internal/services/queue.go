package services

import (
	"fmt"
	"log"
	"time"

	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
)

// QueueService handles queue management operations
type QueueService struct {
	db                              interfaces.DatabaseRepository
	validation                      interfaces.ValidationService
	notification                    interfaces.NotificationService
	config                          interfaces.ConfigService
	suppressIndividualNotifications bool
}

// NewQueueService creates a new queue service
func NewQueueService(
	db interfaces.DatabaseRepository,
	validation interfaces.ValidationService,
	notification interfaces.NotificationService,
	config interfaces.ConfigService,
) interfaces.QueueService {
	return &QueueService{
		db:           db,
		validation:   validation,
		notification: notification,
		config:       config,
	}
}

// SetSuppressIndividualNotifications controls whether individual assignment notifications are sent
func (qs *QueueService) SetSuppressIndividualNotifications(suppress bool) {
	qs.suppressIndividualNotifications = suppress
}

// JoinQueue joins a user to the queue for a specific environment and tag
func (qs *QueueService) JoinQueue(userID, username, environment, tag string, duration time.Duration) error {
	log.Printf("QueueService.JoinQueue: userID=%s, username=%s, env=%s, tag=%s, duration=%v, suppressNotifications=%v",
		userID, username, environment, tag, duration, qs.suppressIndividualNotifications)

	// Get current queue and environments for validation
	queue, err := qs.db.GetQueue()
	if err != nil {
		return fmt.Errorf("failed to get queue: %w", err)
	}

	environments, err := qs.db.GetEnvironments()
	if err != nil {
		return fmt.Errorf("failed to get environments: %w", err)
	}

	// Validate the request
	if err := qs.validation.ValidateQueueSize(len(queue)); err != nil {
		qs.notification.NotifyUserBlock(userID, "🚫", "Queue is Full!", err.Error(), "Please try again later.")
		return err
	}

	if err := qs.validation.ValidateUserInQueueForTag(userID, environment, tag, queue); err != nil {
		qs.notification.NotifyUserBlock(userID, "⏳", "Already in Queue", err.Error(), "Use `@bot status` to check your position.")
		return err
	}

	// Validate environment and tag if specified
	if environment != "" {
		if err := qs.validation.ValidateEnvironmentExists(environment, environments); err != nil {
			qs.notification.NotifyUserBlock(userID, "❌", "Environment Not Found", err.Error(), "Use `@bot list` to see available environments.")
			return err
		}

		if tag != "" {
			if err := qs.validation.ValidateTagExists(environment, tag, environments); err != nil {
				qs.notification.NotifyUserBlock(userID, "❌", "Tag Not Found", err.Error(), "Use `@bot list` to see available tags.")
				return err
			}

			// Check if user is trying to assign a tag they already have
			env := environments[environment]
			tagObj := env.Tags[tag]
			if tagObj.Status == "occupied" && tagObj.AssignedTo == userID {
				msg := fmt.Sprintf("You already have tag '%s' in environment '%s' assigned to you", tag, environment)
				if !tagObj.ExpiresAt.IsZero() {
					timeLeft := tagObj.ExpiresAt.Sub(time.Now())
					if timeLeft > 0 {
						msg += fmt.Sprintf(" (expires in %s)", formatDuration(timeLeft))
					}
				}
				qs.notification.NotifyUserBlock(userID, "🔴", "Already Assigned", msg, "You cannot queue for a tag you already have.")
				return fmt.Errorf("user already has tag %s/%s assigned", environment, tag)
			}

			// If tag is occupied by someone else, allow queueing (don't validate availability)
		}
	}

	// Check for immediate assignment if queue is empty and tags are available
	if isEmpty, _ := qs.db.IsQueueEmpty(environment, tag); isEmpty {
		log.Printf("Queue is empty for %s/%s, checking for immediate assignment", environment, tag)
		availableTags, err := qs.db.GetAvailableTags(environment)
		if err == nil && len(availableTags) > 0 {
			log.Printf("Found %d available tags in %s", len(availableTags), environment)
			for _, availableTag := range availableTags {
				// If specific tag requested, only assign that tag
				if tag != "" && availableTag.Name != tag {
					log.Printf("Skipping %s (requested: %s)", availableTag.Name, tag)
					continue
				}

				log.Printf("Attempting to assign %s/%s to user %s", environment, availableTag.Name, userID)

				// Assign immediately
				assignedAt := time.Now().UTC()
				expiresAt := assignedAt.Add(duration)
				if err := qs.db.UpdateTagStatus(environment, availableTag.Name, "occupied", &userID, &expiresAt); err != nil {
					log.Printf("Failed to update tag status for %s/%s: %v", environment, availableTag.Name, err)
					return fmt.Errorf("failed to assign tag: %w", err)
				}

				log.Printf("Successfully assigned %s/%s to user %s", environment, availableTag.Name, userID)

				// Create updated tag object with proper assignment info for notification
				assignedTag := &models.Tag{
					Name:        availableTag.Name,
					Status:      "occupied",
					AssignedTo:  userID,
					AssignedAt:  assignedAt,
					ExpiresAt:   expiresAt,
					Environment: environment,
				}

				// Notify about immediate assignment (only if not suppressed)
				if !qs.suppressIndividualNotifications {
					qs.notification.NotifyAssignment(userID, assignedTag, environment)
				}

				// Always return early after successful assignment
				// Multi-tag scenarios should handle this at a higher level
				return nil
			}
		} else {
			log.Printf("No available tags found in %s or error: %v", environment, err)
		}
	} else {
		log.Printf("Queue is not empty for %s/%s, will add to queue", environment, tag)
	}

	// Add to queue
	durationMinutes := int(duration.Minutes())
	if err := qs.db.JoinQueue(userID, username, environment, tag, durationMinutes); err != nil {
		log.Printf("Failed to join queue for %s/%s: %v", environment, tag, err)
		return fmt.Errorf("failed to join queue: %w", err)
	}

	log.Printf("Successfully added user %s to queue for %s/%s", userID, environment, tag)

	// Notify user about queue position
	position := qs.GetUserPosition(userID)
	qs.notification.NotifyQueueJoined(userID, position.Position)

	return nil
}

// LeaveQueue removes a user from the queue
func (qs *QueueService) LeaveQueue(userID string) error {
	log.Printf("QueueService.LeaveQueue: userID=%s", userID)

	// Remove from queue
	if err := qs.db.LeaveQueue(userID); err != nil {
		return fmt.Errorf("failed to leave queue: %w", err)
	}

	qs.notification.NotifyQueueLeft(userID)

	return nil
}

// GetUserPosition returns the position of a user in the queue
func (qs *QueueService) GetUserPosition(userID string) *models.UserPosition {
	position, queueItem, err := qs.db.GetUserPosition(userID)
	if err != nil {
		log.Printf("Error getting user position: %v", err)
		return &models.UserPosition{
			Position:          0,
			QueueItem:         nil,
			EstimatedWait:     0,
			ActiveAssignment:  nil,
			ActiveAssignments: []*models.Tag{},
		}
	}

	userPos := &models.UserPosition{
		Position:  position,
		QueueItem: queueItem,
	}

	// Get ALL user assignments from environments
	environments, err := qs.db.GetEnvironments()
	if err == nil {
		var activeAssignments []*models.Tag
		for _, env := range environments {
			for _, tag := range env.Tags {
				if tag.AssignedTo == userID && tag.Status == "occupied" {
					activeAssignments = append(activeAssignments, tag)
				}
			}
		}
		userPos.ActiveAssignments = activeAssignments

		// Set ActiveAssignment to first assignment for backward compatibility
		if len(activeAssignments) > 0 {
			userPos.ActiveAssignment = activeAssignments[0]
		}

		// If user has assignments, set position to -1
		if len(activeAssignments) > 0 {
			userPos.Position = -1
		}
	}

	// If user is in queue, calculate estimated wait time
	if position > 0 && queueItem != nil {
		// Rough estimate: 30 minutes per person ahead
		estimatedWaitMinutes := (position - 1) * 30
		userPos.EstimatedWait = time.Duration(estimatedWaitMinutes) * time.Minute
	}

	return userPos
}

// GetQueueStatus returns the current queue status
func (qs *QueueService) GetQueueStatus() *models.QueueStatus {
	log.Println("QueueService.GetQueueStatus: Getting current queue status")

	// Get current data
	queue, err := qs.db.GetQueue()
	if err != nil {
		log.Printf("Error getting queue: %v", err)
		queue = []models.QueueItem{}
	}

	environments, err := qs.db.GetEnvironments()
	if err != nil {
		log.Printf("Error getting environments: %v", err)
		environments = make(map[string]*models.Environment)
	}

	// Count available and occupied tags
	availableTags := 0
	occupiedTags := 0
	for _, env := range environments {
		for _, tag := range env.Tags {
			switch tag.Status {
			case "available":
				availableTags++
			case "occupied":
				occupiedTags++
			}
		}
	}

	return &models.QueueStatus{
		Queue:         queue,
		Environments:  environments,
		TotalUsers:    len(queue),
		AvailableTags: availableTags,
		OccupiedTags:  occupiedTags,
	}
}

// ProcessQueue processes the queue and assigns available tags
func (qs *QueueService) ProcessQueue() error {
	log.Println("QueueService.ProcessQueue: Processing queue for available assignments")

	queue, err := qs.db.GetQueue()
	if err != nil {
		return fmt.Errorf("failed to get queue: %w", err)
	}

	if len(queue) == 0 {
		return nil // Nothing to process
	}

	// Try to assign the first user in queue
	firstItem := queue[0]

	// Find available tags in the requested environment
	availableTags, err := qs.db.GetAvailableTags(firstItem.Environment)
	if err != nil || len(availableTags) == 0 {
		return nil // No tags available
	}

	// Find a suitable tag
	var targetTag *models.Tag
	for _, tag := range availableTags {
		// If user requested specific tag, find exact match
		if firstItem.Tag != "" {
			if tag.Name == firstItem.Tag {
				targetTag = tag
				break
			}
		} else {
			// User wants any tag in the environment
			targetTag = tag
			break
		}
	}

	if targetTag != nil {
		// Assign the tag
		assignedAt := time.Now().UTC()
		expiresAt := assignedAt.Add(firstItem.Duration)
		if err := qs.db.UpdateTagStatus(targetTag.Environment, targetTag.Name, "occupied", &firstItem.UserID, &expiresAt); err != nil {
			return fmt.Errorf("failed to assign tag during queue processing: %w", err)
		}

		// Remove user from queue
		if err := qs.db.LeaveQueue(firstItem.UserID); err != nil {
			log.Printf("Warning: Failed to remove user from queue after assignment: %v", err)
		}

		// Create updated tag object with proper assignment info for notification
		assignedTag := &models.Tag{
			Name:        targetTag.Name,
			Status:      "occupied",
			AssignedTo:  firstItem.UserID,
			AssignedAt:  assignedAt,
			ExpiresAt:   expiresAt,
			Environment: targetTag.Environment,
		}

		// Notify about assignment (only if not suppressed)
		if !qs.suppressIndividualNotifications {
			qs.notification.NotifyAssignment(firstItem.UserID, assignedTag, targetTag.Environment)
		}

		log.Printf("Assigned %s/%s to user %s during queue processing", targetTag.Environment, targetTag.Name, firstItem.UserID)
	}

	return nil
}

// IsQueueEmpty returns true if the queue is empty for a specific environment/tag
func (qs *QueueService) IsQueueEmpty(environment, tag string) bool {
	isEmpty, err := qs.db.IsQueueEmpty(environment, tag)
	if err != nil {
		log.Printf("Error checking if queue is empty: %v", err)
		return false
	}
	return isEmpty
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "less than 1 minute"
	}
	if d < time.Hour {
		minutes := int(d.Minutes())
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if minutes == 0 {
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}
	if hours == 1 {
		return fmt.Sprintf("1 hour %d minutes", minutes)
	}
	return fmt.Sprintf("%d hours %d minutes", hours, minutes)
}

// ReleaseUserTags releases all tags assigned to a user
func (qs *QueueService) ReleaseUserTags(userID string) ([]string, error) {
	log.Printf("QueueService.ReleaseUserTags: userID=%s", userID)

	// Get environments to find user assignments
	environments, err := qs.db.GetEnvironments()
	if err != nil {
		return nil, fmt.Errorf("failed to get environments: %w", err)
	}

	var releasedTags []string
	var errors []string

	// Find and release all user assignments
	for _, env := range environments {
		for _, tag := range env.Tags {
			if tag.AssignedTo == userID && tag.Status == "occupied" {
				// Release the tag
				releaseErr := qs.db.UpdateTagStatus(env.Name, tag.Name, "available", nil, nil)
				if releaseErr != nil {
					errors = append(errors, fmt.Sprintf("failed to release %s/%s: %v", env.Name, tag.Name, releaseErr))
					continue
				}

				releasedTags = append(releasedTags, fmt.Sprintf("%s/%s", env.Name, tag.Name))
				log.Printf("Released tag %s/%s for user %s", env.Name, tag.Name, userID)

				// Send notification about releasing the tag
				qs.notification.NotifyTagRelease(userID, env.Name, tag.Name)
			}
		}
	}

	if len(errors) > 0 {
		return releasedTags, fmt.Errorf("some releases failed: %s", fmt.Sprintf("%v", errors))
	}

	if len(releasedTags) == 0 {
		return nil, fmt.Errorf("no active assignments found to release")
	}

	// Process queue to potentially assign newly available tags
	go qs.ProcessQueue()

	return releasedTags, nil
}

// ReleaseUserTagsBulk releases all tags for a user without individual notifications (for bulk operations)
func (qs *QueueService) ReleaseUserTagsBulk(userID string) ([]string, error) {
	log.Printf("QueueService.ReleaseUserTagsBulk: userID=%s", userID)

	// Get environments to find user assignments
	environments, err := qs.db.GetEnvironments()
	if err != nil {
		return nil, fmt.Errorf("failed to get environments: %w", err)
	}

	var releasedTags []string
	var errors []string

	// Find and release all user assignments without individual notifications
	for _, env := range environments {
		for _, tag := range env.Tags {
			if tag.AssignedTo == userID && tag.Status == "occupied" {
				// Release the tag
				releaseErr := qs.db.UpdateTagStatus(env.Name, tag.Name, "available", nil, nil)
				if releaseErr != nil {
					errors = append(errors, fmt.Sprintf("failed to release %s/%s: %v", env.Name, tag.Name, releaseErr))
					continue
				}

				releasedTags = append(releasedTags, fmt.Sprintf("%s/%s", env.Name, tag.Name))
				log.Printf("Released tag %s/%s for user %s (bulk operation)", env.Name, tag.Name, userID)
			}
		}
	}

	if len(errors) > 0 {
		return releasedTags, fmt.Errorf("some releases failed: %s", fmt.Sprintf("%v", errors))
	}

	if len(releasedTags) == 0 {
		return nil, fmt.Errorf("no active assignments found to release")
	}

	// Process queue to potentially assign newly available tags
	go qs.ProcessQueue()

	return releasedTags, nil
}

// ReleaseSpecificTag releases a specific tag for a user
func (qs *QueueService) ReleaseSpecificTag(userID, environment, tagName string) error {
	log.Printf("QueueService.ReleaseSpecificTag: userID=%s, env=%s, tag=%s", userID, environment, tagName)

	// Get the tag to verify the user has it assigned
	environments, err := qs.db.GetEnvironments()
	if err != nil {
		return fmt.Errorf("failed to get environments: %w", err)
	}

	env, exists := environments[environment]
	if !exists {
		return fmt.Errorf("environment '%s' not found", environment)
	}

	tag, exists := env.Tags[tagName]
	if !exists {
		return fmt.Errorf("tag '%s' not found in environment '%s'", tagName, environment)
	}

	if tag.Status != "occupied" || tag.AssignedTo != userID {
		return fmt.Errorf("tag '%s/%s' is not assigned to you", environment, tagName)
	}

	// Release the tag
	err = qs.db.UpdateTagStatus(environment, tagName, "available", nil, nil)
	if err != nil {
		return fmt.Errorf("failed to release tag: %w", err)
	}

	// Send notification about releasing the tag
	qs.notification.NotifyTagRelease(userID, environment, tagName)

	// Process queue to potentially assign to someone else
	go qs.ProcessQueue()

	log.Printf("Released tag %s/%s for user %s", environment, tagName, userID)
	return nil
}

// ClearQueue removes all users from the queue (admin function)
func (qs *QueueService) ClearQueue() (int, error) {
	log.Printf("QueueService.ClearQueue: Admin clearing all queue positions")

	count, err := qs.db.ClearQueue()
	if err != nil {
		return 0, fmt.Errorf("failed to clear queue: %w", err)
	}

	log.Printf("Cleared %d users from queue", count)
	return count, nil
}

// ReleaseAllAssignedTags releases all currently assigned tags (admin function)
func (qs *QueueService) ReleaseAllAssignedTags() (int, []string, error) {
	log.Printf("QueueService.ReleaseAllAssignedTags: Admin releasing all assigned tags")

	count, tagNames, err := qs.db.ReleaseAllTags()
	if err != nil {
		return 0, nil, fmt.Errorf("failed to release all tags: %w", err)
	}

	log.Printf("Released %d tags: %v", count, tagNames)

	// Process queue to potentially assign newly available tags
	if count > 0 {
		go qs.ProcessQueue()
	}

	return count, tagNames, nil
}
