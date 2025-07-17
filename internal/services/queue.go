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
	db           interfaces.DatabaseRepository
	validation   interfaces.ValidationService
	notification interfaces.NotificationService
	config       interfaces.ConfigService
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

// JoinQueue adds a user to the queue for a specific tag
func (qs *QueueService) JoinQueue(userID, username, environment, tag string, duration time.Duration) error {
	log.Printf("QueueService.JoinQueue: userID=%s, username=%s, env=%s, tag=%s, duration=%v",
		userID, username, environment, tag, duration)

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
		qs.notification.NotifyUserBlock(userID, "🚫", "Queue is Full!", err.Error(), "Please try again later or ask an admin for help.")
		return err
	}

	if err := qs.validation.ValidateUserInQueue(userID, queue); err != nil {
		qs.notification.NotifyUserBlock(userID, "ℹ️", "Already in Queue", err.Error(), "Use `@bot position` to check your spot or `@bot leave` to exit.")
		return err
	}

	if err := qs.validation.ValidateDuration(duration); err != nil {
		validOptions := []string{"30m", "1h", "1h30m", "2h", "2h30m", "3h"}
		qs.notification.NotifyUserBlock(userID, "⚠️", "Invalid Duration", err.Error(), "Valid options: "+fmt.Sprintf("%v", validOptions))
		return err
	}

	if err := qs.validation.ValidateEnvironmentExists(environment, environments); err != nil {
		qs.notification.NotifyUserBlock(userID, "❌", "Environment Not Found", err.Error(), "Use `@bot list` to see available environments.")
		return err
	}

	if tag != "" {
		if err := qs.validation.ValidateTagExists(environment, tag, environments); err != nil {
			qs.notification.NotifyUserBlock(userID, "❌", "Tag Not Found", err.Error(), "Use `@bot list` to see available tags.")
			return err
		}

		if err := qs.validation.ValidateTagAvailability(environment, tag, environments); err != nil {
			env := environments[environment]
			tagObj := env.Tags[tag]
			msg := fmt.Sprintf("Tag '%s' in environment '%s' is not available (status: %s)", tag, environment, tagObj.Status)
			suggestion := ""
			if tagObj.AssignedTo != "" {
				msg += fmt.Sprintf(". Currently assigned to <@%s>", tagObj.AssignedTo)
				if !tagObj.ExpiresAt.IsZero() {
					timeLeft := tagObj.ExpiresAt.Sub(time.Now())
					if timeLeft > 0 {
						msg += fmt.Sprintf(" (expires in %s)", formatDuration(timeLeft))
					}
				}
				suggestion = "Try another tag or wait until it becomes available."
			}
			qs.notification.NotifyUserBlock(userID, "🔴", "Tag Unavailable", msg, suggestion)
			return err
		}
	}

	// Check for immediate assignment if queue is empty and tags are available
	if isEmpty, _ := qs.db.IsQueueEmpty(environment, tag); isEmpty {
		availableTags, err := qs.db.GetAvailableTags(environment)
		if err == nil && len(availableTags) > 0 {
			for _, availableTag := range availableTags {
				// If specific tag requested, only assign that tag
				if tag != "" && availableTag.Name != tag {
					continue
				}

				// Assign immediately
				expiresAt := time.Now().Add(duration)
				if err := qs.db.UpdateTagStatus(environment, availableTag.Name, "occupied", &userID, &expiresAt); err != nil {
					return fmt.Errorf("failed to assign tag: %w", err)
				}

				// Notify about immediate assignment
				qs.notification.NotifyAssignment(userID, availableTag, availableTag.Environment)
				qs.notification.BroadcastQueueUpdate()

				return nil
			}
		}
	}

	// Add to queue
	durationMinutes := int(duration.Minutes())
	if err := qs.db.JoinQueue(userID, username, environment, tag, durationMinutes); err != nil {
		return fmt.Errorf("failed to join queue: %w", err)
	}

	// Notify user about queue position
	position := qs.GetUserPosition(userID)
	qs.notification.NotifyQueueJoined(userID, position.Position)
	qs.notification.BroadcastQueueUpdate()

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
	qs.notification.BroadcastQueueUpdate()

	return nil
}

// GetUserPosition returns the position of a user in the queue
func (qs *QueueService) GetUserPosition(userID string) *models.UserPosition {
	position, queueItem, err := qs.db.GetUserPosition(userID)
	if err != nil {
		log.Printf("Error getting user position: %v", err)
		return &models.UserPosition{
			Position:         0,
			QueueItem:        nil,
			EstimatedWait:    0,
			ActiveAssignment: nil,
		}
	}

	userPos := &models.UserPosition{
		Position:  position,
		QueueItem: queueItem,
	}

	// If user has active assignment
	if position == -1 {
		// Get user's assignment from environments
		environments, err := qs.db.GetEnvironments()
		if err == nil {
			for _, env := range environments {
				for _, tag := range env.Tags {
					if tag.AssignedTo == userID && tag.Status == "occupied" {
						userPos.ActiveAssignment = tag
						break
					}
				}
			}
		}
		return userPos
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
		expiresAt := time.Now().Add(firstItem.Duration)
		if err := qs.db.UpdateTagStatus(targetTag.Environment, targetTag.Name, "occupied", &firstItem.UserID, &expiresAt); err != nil {
			return fmt.Errorf("failed to assign tag during queue processing: %w", err)
		}

		// Remove user from queue
		if err := qs.db.LeaveQueue(firstItem.UserID); err != nil {
			log.Printf("Warning: Failed to remove user from queue after assignment: %v", err)
		}

		// Notify about assignment
		qs.notification.NotifyAssignment(firstItem.UserID, targetTag, targetTag.Environment)
		qs.notification.BroadcastQueueUpdate()

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
