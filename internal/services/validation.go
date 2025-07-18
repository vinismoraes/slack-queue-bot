package services

import (
	"fmt"
	"strings"
	"time"

	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
	"slack-queue-bot/internal/utils"
)

// ValidationService handles all input validation
type ValidationService struct {
	config *models.ConfigSettings
}

// NewValidationService creates a new validation service
func NewValidationService(config *models.ConfigSettings) interfaces.ValidationService {
	return &ValidationService{
		config: config,
	}
}

// ValidateDuration validates if a duration is within acceptable bounds
func (v *ValidationService) ValidateDuration(duration time.Duration) error {
	return utils.ValidateDuration(duration, v.config.MinDuration.ToDuration(), v.config.MaxDuration.ToDuration())
}

// ValidateQueueSize validates if the queue size is within limits
func (v *ValidationService) ValidateQueueSize(currentSize int) error {
	if currentSize >= v.config.MaxQueueSize {
		return fmt.Errorf("queue is full (%d/%d)", currentSize, v.config.MaxQueueSize)
	}

	// Warning if queue is almost full
	if currentSize >= v.config.MaxQueueSize-2 {
		return fmt.Errorf("queue is almost full (%d/%d)", currentSize, v.config.MaxQueueSize)
	}

	return nil
}

// ValidateUserInQueue checks if a user is already in the queue for any environment/tag
func (v *ValidationService) ValidateUserInQueue(userID string, queue []models.QueueItem) error {
	for _, item := range queue {
		if item.UserID == userID {
			return fmt.Errorf("already in queue for %s/%s", item.Environment, item.Tag)
		}
	}
	return nil
}

// ValidateUserInQueueForTag checks if a user is already in queue for a specific environment/tag
func (v *ValidationService) ValidateUserInQueueForTag(userID, environment, tag string, queue []models.QueueItem) error {
	for _, item := range queue {
		if item.UserID == userID && item.Environment == environment && item.Tag == tag {
			return fmt.Errorf("already in queue for %s/%s", environment, tag)
		}
	}
	return nil
}

// ValidateEnvironmentExists checks if an environment exists
func (v *ValidationService) ValidateEnvironmentExists(environment string, environments map[string]*models.Environment) error {
	if _, exists := environments[environment]; !exists {
		return fmt.Errorf("environment '%s' does not exist", environment)
	}
	return nil
}

// ValidateTagExists checks if a tag exists in an environment
func (v *ValidationService) ValidateTagExists(environment, tag string, environments map[string]*models.Environment) error {
	env, exists := environments[environment]
	if !exists {
		return fmt.Errorf("environment '%s' does not exist", environment)
	}

	if tag == "" {
		return nil // Empty tag means any tag in environment
	}

	_, tagExists := env.Tags[tag]
	if !tagExists {
		return fmt.Errorf("tag '%s' does not exist in environment '%s'", tag, environment)
	}

	return nil
}

// ValidateTagAvailability checks if a tag is available for assignment
func (v *ValidationService) ValidateTagAvailability(environment, tag string, environments map[string]*models.Environment) error {
	if err := v.ValidateTagExists(environment, tag, environments); err != nil {
		return err
	}

	if tag == "" {
		return nil // Any tag validation handled elsewhere
	}

	env := environments[environment]
	tagObj := env.Tags[tag]

	if tagObj.Status != "available" {
		msg := fmt.Sprintf("tag '%s' in environment '%s' is not available (status: %s)", tag, environment, tagObj.Status)

		if tagObj.AssignedTo != "" {
			msg += fmt.Sprintf(", currently assigned to %s", tagObj.AssignedTo)
			if !tagObj.ExpiresAt.IsZero() {
				timeLeft := tagObj.ExpiresAt.Sub(time.Now())
				if timeLeft > 0 {
					msg += fmt.Sprintf(" (expires in %s)", utils.FormatDuration(timeLeft))
				}
			}
		}

		return fmt.Errorf("%s", msg)
	}

	return nil
}

// ValidateUserAssignment checks if a user has an active assignment to a specific tag
func (v *ValidationService) ValidateUserAssignment(userID, environment, tag string, environments map[string]*models.Environment) error {
	if err := v.ValidateTagExists(environment, tag, environments); err != nil {
		return err
	}

	env := environments[environment]
	tagObj := env.Tags[tag]

	if tagObj.Status != "occupied" || tagObj.AssignedTo != userID {
		return fmt.Errorf("you are not assigned to tag '%s' in environment '%s'", tag, environment)
	}

	return nil
}

// ValidateCommand validates a command request structure
func (v *ValidationService) ValidateCommand(cmd *models.CommandRequest) error {
	if cmd.UserID == "" {
		return fmt.Errorf("user ID is required")
	}

	if cmd.Username == "" {
		return fmt.Errorf("username is required")
	}

	if cmd.Command == "" {
		return fmt.Errorf("command is required")
	}

	// Validate command-specific requirements
	switch cmd.Command {
	case "join":
		if cmd.Environment == "" {
			return fmt.Errorf("environment is required for join command")
		}
		if cmd.Duration == 0 {
			return fmt.Errorf("duration is required for join command")
		}
		return v.ValidateDuration(cmd.Duration)

	case "release", "extend":
		if cmd.Environment == "" {
			return fmt.Errorf("environment is required for %s command", cmd.Command)
		}
		if cmd.Tag == "" {
			return fmt.Errorf("tag is required for %s command", cmd.Command)
		}

	case "leave", "position", "status", "list", "assign", "help", "cleanup", "clear", "admin":
		// These commands don't require additional validation

	default:
		return fmt.Errorf("unknown command: %s", cmd.Command)
	}

	return nil
}

// GetValidationErrors creates user-friendly error messages with suggestions
func (v *ValidationService) GetValidationErrors() map[string]models.NotificationMessage {
	return map[string]models.NotificationMessage{
		"queue_full": {
			Emoji:      "🚫",
			Summary:    "Queue is Full!",
			Detail:     fmt.Sprintf("The queue is currently at its limit (%d/%d).", v.config.MaxQueueSize, v.config.MaxQueueSize),
			Suggestion: "Please try again later or ask an admin for help.",
			IsBlock:    true,
		},
		"already_in_queue": {
			Emoji:      "ℹ️",
			Summary:    "Already in Queue",
			Detail:     "You are already in the queue.",
			Suggestion: "Use `@bot position` to check your spot or `@bot leave` to exit.",
			IsBlock:    true,
		},
		"duration_too_short": {
			Emoji:      "⚠️",
			Summary:    "Duration Too Short",
			Detail:     fmt.Sprintf("Minimum duration is %s.", utils.FormatDuration(v.config.MinDuration.ToDuration())),
			Suggestion: "Please specify a longer duration.",
			IsBlock:    true,
		},
		"duration_too_long": {
			Emoji:      "⚠️",
			Summary:    "Duration Too Long",
			Detail:     fmt.Sprintf("Maximum duration is %s.", utils.FormatDuration(v.config.MaxDuration.ToDuration())),
			Suggestion: "Please specify a shorter duration.",
			IsBlock:    true,
		},
		"invalid_duration": {
			Emoji:      "⚠️",
			Summary:    "Invalid Duration",
			Detail:     "Duration must be in 30-minute intervals.",
			Suggestion: "Valid options: " + strings.Join(utils.GetValidDurations(), ", "),
			IsBlock:    true,
		},
		"environment_not_found": {
			Emoji:      "❌",
			Summary:    "Environment Not Found",
			Detail:     "The specified environment does not exist.",
			Suggestion: "Use `@bot list` to see available environments.",
			IsBlock:    true,
		},
		"tag_not_found": {
			Emoji:      "❌",
			Summary:    "Tag Not Found",
			Detail:     "The specified tag does not exist in this environment.",
			Suggestion: "Use `@bot list` to see available tags.",
			IsBlock:    true,
		},
		"tag_unavailable": {
			Emoji:      "🔴",
			Summary:    "Tag Unavailable",
			Detail:     "The requested tag is not available.",
			Suggestion: "Try another tag or wait until it becomes available.",
			IsBlock:    true,
		},
		"no_assignment": {
			Emoji:      "❌",
			Summary:    "No Assignment",
			Detail:     "You are not assigned to the specified tag.",
			Suggestion: "Check your active assignments with `@bot position`.",
			IsBlock:    true,
		},
		"not_in_queue": {
			Emoji:      "❌",
			Summary:    "Not in Queue",
			Detail:     "You are not currently in the queue.",
			Suggestion: "Use `@bot join` to join the queue.",
			IsBlock:    true,
		},
	}
}
