package services

import (
	"fmt"
	"log"
	"time"

	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
	"slack-queue-bot/internal/utils"

	"github.com/slack-go/slack"
)

// NotificationService handles all Slack messaging operations
type NotificationService struct {
	slackClient  *slack.Client
	channelID    string
	queueService interfaces.QueueService
	config       *models.ConfigSettings
}

// NewNotificationService creates a new notification service
func NewNotificationService(
	slackClient *slack.Client,
	channelID string,
	config *models.ConfigSettings,
) interfaces.NotificationService {
	return &NotificationService{
		slackClient: slackClient,
		channelID:   channelID,
		config:      config,
	}
}

// SetQueueService sets the queue service reference (to avoid circular dependency)
func (ns *NotificationService) SetQueueService(queueService interfaces.QueueService) {
	ns.queueService = queueService
}

// NotifyUser sends a simple text message to a user
func (ns *NotificationService) NotifyUser(userID, message string) error {
	log.Printf("NotificationService.NotifyUser: userID=%s", userID)

	_, _, err := ns.slackClient.PostMessage(userID, slack.MsgOptionText(message, false))
	if err != nil {
		return fmt.Errorf("failed to send message to user %s: %w", userID, err)
	}

	return nil
}

// NotifyUserBlock sends a structured block message to a user
func (ns *NotificationService) NotifyUserBlock(userID, emoji, summary, detail, suggestion string) error {
	log.Printf("NotificationService.NotifyUserBlock: userID=%s, summary=%s", userID, summary)

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("%s *%s*\n%s", emoji, summary, detail), false, false),
			nil, nil,
		),
	}

	if suggestion != "" {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", suggestion, false, false)))
	}

	_, _, err := ns.slackClient.PostMessage(userID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		return fmt.Errorf("failed to send block message to user %s: %w", userID, err)
	}

	return nil
}

// BroadcastQueueUpdate sends current queue status to the channel
func (ns *NotificationService) BroadcastQueueUpdate() error {
	log.Printf("NotificationService.BroadcastQueueUpdate")

	if ns.queueService == nil {
		return fmt.Errorf("queue service not set")
	}

	status := ns.queueService.GetQueueStatus()
	blocks := ns.createQueueStatusBlocks(status)

	_, _, err := ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		return fmt.Errorf("failed to broadcast queue update: %w", err)
	}

	return nil
}

// CreateQueueStatusBlocks creates and returns status blocks without sending them
// This allows for ephemeral responses or custom message handling
func (ns *NotificationService) CreateQueueStatusBlocks() ([]slack.Block, error) {
	log.Printf("NotificationService.CreateQueueStatusBlocks")

	if ns.queueService == nil {
		return nil, fmt.Errorf("queue service not set")
	}

	status := ns.queueService.GetQueueStatus()
	blocks := ns.createQueueStatusBlocks(status)

	return blocks, nil
}

// NotifyAssignment notifies a user of tag assignment
func (ns *NotificationService) NotifyAssignment(userID string, tag *models.Tag, environment string) error {
	log.Printf("NotificationService.NotifyAssignment: userID=%s, tag=%s/%s", userID, environment, tag.Name)

	expiryTime := tag.ExpiresAt.Format("15:04")
	duration := tag.ExpiresAt.Sub(tag.AssignedAt)
	durationText := utils.FormatDuration(duration)

	message := fmt.Sprintf("🎉 *Tag Assigned!*\n"+
		"You've been assigned to tag: *%s* in environment: *%s*\n"+
		"⏰ This assignment expires at *%s* (%s from now)",
		tag.Name, environment, expiryTime, durationText)

	// Send DM to user
	err := ns.NotifyUser(userID, message)
	if err != nil {
		log.Printf("Warning: Failed to send assignment DM to user %s: %v", userID, err)
	}

	// Send channel notification
	channelMessage := fmt.Sprintf("🎉 <@%s> has been assigned to *%s/%s* (expires at %s)",
		userID, environment, tag.Name, expiryTime)

	_, _, err = ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(channelMessage, false))
	if err != nil {
		log.Printf("Warning: Failed to send assignment channel message: %v", err)
	}

	return nil
}

// NotifyExpiration notifies about tag expiration
func (ns *NotificationService) NotifyExpiration(userID string, tag *models.Tag, environment string) error {
	log.Printf("NotificationService.NotifyExpiration: userID=%s, tag=%s/%s", userID, environment, tag.Name)

	// Send DM to user
	message := fmt.Sprintf("⏰ *Assignment Expired*\n"+
		"Your assignment to tag *%s* in environment *%s* has expired and has been released.",
		tag.Name, environment)

	err := ns.NotifyUser(userID, message)
	if err != nil {
		log.Printf("Warning: Failed to send expiration DM to user %s: %v", userID, err)
	}

	// Send channel notification
	channelMessage := fmt.Sprintf("⏰ <@%s>'s assignment to *%s/%s* has expired and been released",
		userID, environment, tag.Name)

	_, _, err = ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(channelMessage, false))
	if err != nil {
		log.Printf("Warning: Failed to send expiration channel message: %v", err)
	}

	return nil
}

// NotifyQueueJoined notifies a user they've joined the queue
func (ns *NotificationService) NotifyQueueJoined(userID string, position int) error {
	log.Printf("NotificationService.NotifyQueueJoined: userID=%s, position=%d", userID, position)

	var message string
	if position == 1 {
		message = "✅ *Added to Queue!*\n🎯 You're next in line! You'll be assigned as soon as a tag becomes available."
	} else {
		estimatedWaitMinutes := (position - 1) * 30
		estimatedWait := time.Duration(estimatedWaitMinutes) * time.Minute
		message = fmt.Sprintf("✅ *Added to Queue!*\n"+
			"📋 You are position *%d* in the queue\n"+
			"⏱️ Estimated wait time: *%s*",
			position, utils.FormatDuration(estimatedWait))
	}

	return ns.NotifyUser(userID, message)
}

// NotifyQueueLeft notifies a user they've left the queue
func (ns *NotificationService) NotifyQueueLeft(userID string) error {
	log.Printf("NotificationService.NotifyQueueLeft: userID=%s", userID)

	message := "👋 *Left Queue*\nYou have been removed from the queue."

	// Send DM to user
	err := ns.NotifyUser(userID, message)
	if err != nil {
		log.Printf("Warning: Failed to send queue left DM to user %s: %v", userID, err)
	}

	// Send channel notification
	channelMessage := fmt.Sprintf("👋 <@%s> has left the queue", userID)

	_, _, err = ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(channelMessage, false))
	if err != nil {
		log.Printf("Warning: Failed to send queue left channel message: %v", err)
	}

	return nil
}

// NotifyTagRelease notifies about manual tag release
func (ns *NotificationService) NotifyTagRelease(userID string, environment, tag string) error {
	log.Printf("NotificationService.NotifyTagRelease: userID=%s, tag=%s/%s", userID, environment, tag)

	// Send DM to user
	message := fmt.Sprintf("✅ *Tag Released*\n"+
		"You have successfully released tag *%s* in environment *%s*.",
		tag, environment)

	err := ns.NotifyUser(userID, message)
	if err != nil {
		log.Printf("Warning: Failed to send release DM to user %s: %v", userID, err)
	}

	// Send channel notification
	channelMessage := fmt.Sprintf("✅ <@%s> has released *%s/%s*", userID, environment, tag)

	_, _, err = ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(channelMessage, false))
	if err != nil {
		log.Printf("Warning: Failed to send release channel message: %v", err)
	}

	return nil
}

// NotifyAssignmentExtended notifies about assignment extension
func (ns *NotificationService) NotifyAssignmentExtended(userID string, tag *models.Tag, environment string, extension time.Duration) error {
	log.Printf("NotificationService.NotifyAssignmentExtended: userID=%s, tag=%s/%s, extension=%v", userID, environment, tag.Name, extension)

	newExpiryTime := tag.ExpiresAt.Format("15:04")
	extensionText := utils.FormatDuration(extension)

	message := fmt.Sprintf("⏰ *Assignment Extended*\n"+
		"Your assignment to tag *%s* in environment *%s* has been extended by *%s*.\n"+
		"New expiration time: *%s*",
		tag.Name, environment, extensionText, newExpiryTime)

	return ns.NotifyUser(userID, message)
}

// createQueueStatusBlocks creates Slack blocks for queue status display
func (ns *NotificationService) createQueueStatusBlocks(status *models.QueueStatus) []slack.Block {
	blocks := []slack.Block{}

	// Show warning if queue is full or nearly full
	if status.TotalUsers >= ns.config.MaxQueueSize {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf(":warning: *Queue is full!* (%d/%d). Please try again later or ask an admin for help.",
					status.TotalUsers, ns.config.MaxQueueSize), false, false),
		))
	} else if status.TotalUsers >= ns.config.MaxQueueSize-2 {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf(":warning: *Queue is almost full!* (%d/%d)",
					status.TotalUsers, ns.config.MaxQueueSize), false, false),
		))
	}

	// Header
	blocks = append(blocks, slack.NewHeaderBlock(
		slack.NewTextBlockObject("plain_text", "📋 Test Environment Queue", false, false)))

	// Add divider and spacing before summary
	blocks = append(blocks, slack.NewDividerBlock())

	// Summary section - very concise
	totalAvailable := 0
	totalOccupied := 0
	totalMaintenance := 0
	for _, env := range status.Environments {
		for _, tag := range env.Tags {
			switch tag.Status {
			case "available":
				totalAvailable++
			case "occupied":
				totalOccupied++
			case "maintenance":
				totalMaintenance++
			}
		}
	}

	summaryText := fmt.Sprintf("🟢 *%d* available • 🔴 *%d* occupied • 👥 *%d* in queue",
		totalAvailable, totalOccupied, status.TotalUsers)

	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", summaryText, false, false),
		nil, nil,
	))

	// Occupied tags section - only show if there are occupied tags
	if totalOccupied > 0 {
		blocks = append(blocks, slack.NewDividerBlock())

		occupiedText := "*🔴 Occupied Tags:*\n"

		for envName, env := range status.Environments {
			envHasOccupied := false
			var envOccupiedTags []string

			for _, tag := range env.Tags {
				if tag.Status == "occupied" {
					envHasOccupied = true
					tagInfo := fmt.Sprintf("• `%s`", tag.Name)

					if tag.AssignedTo != "" {
						tagInfo += fmt.Sprintf(" - <@%s>", tag.AssignedTo)
					}

					if !tag.ExpiresAt.IsZero() {
						timeLeft := tag.ExpiresAt.Sub(time.Now())
						if timeLeft > 0 {
							tagInfo += fmt.Sprintf(" (expires in %s)", utils.FormatDuration(timeLeft))
						} else {
							tagInfo += " (expired)"
						}
					}

					envOccupiedTags = append(envOccupiedTags, tagInfo)
				}
			}

			if envHasOccupied {
				occupiedText += fmt.Sprintf("*%s:*\n", envName)
				for _, tagInfo := range envOccupiedTags {
					occupiedText += tagInfo + "\n"
				}
				occupiedText += "\n"
			}
		}

		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", occupiedText, false, false),
			nil, nil,
		))
	}

	// Queue section - only show if there are users waiting
	if len(status.Queue) > 0 {
		blocks = append(blocks, slack.NewDividerBlock())

		queueText := "*👥 Queue:*\n"
		// Show only first 3 users
		displayCount := len(status.Queue)
		if displayCount > 3 {
			displayCount = 3
		}

		for i := 0; i < displayCount; i++ {
			item := status.Queue[i]
			position := i + 1
			queueText += fmt.Sprintf("%d. <@%s>", position, item.UserID)
			if item.Environment != "" && item.Tag != "" {
				queueText += fmt.Sprintf(" (waiting for %s/%s)", item.Environment, item.Tag)
			} else if item.Environment != "" {
				queueText += fmt.Sprintf(" (waiting for %s)", item.Environment)
			}
			queueText += "\n"
		}

		if len(status.Queue) > 3 {
			queueText += fmt.Sprintf("... and %d more", len(status.Queue)-3)
		}

		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", queueText, false, false),
			nil, nil,
		))
	}

	// Add divider before buttons
	blocks = append(blocks, slack.NewDividerBlock())

	// Interactive Buttons
	joinButton := slack.NewButtonBlockElement("join_queue", "join_queue",
		slack.NewTextBlockObject("plain_text", "🎯 Join Queue", true, false))
	joinButton.Style = "primary"

	leaveButton := slack.NewButtonBlockElement("leave_queue", "leave_queue",
		slack.NewTextBlockObject("plain_text", "🚪 Leave Queue", true, false))

	statusButton := slack.NewButtonBlockElement("check_position", "check_position",
		slack.NewTextBlockObject("plain_text", "📍 My Position", true, false))

	actionBlock := slack.NewActionBlock("queue_actions", joinButton, leaveButton, statusButton)
	blocks = append(blocks, actionBlock)

	return blocks
}

// NotifyUpcomingExpirations notifies about tags expiring soon
func (ns *NotificationService) NotifyUpcomingExpirations(tags []*models.Tag, window time.Duration) error {
	log.Printf("NotificationService.NotifyUpcomingExpirations: %d tags expiring within %v", len(tags), window)

	if len(tags) == 0 {
		return nil
	}

	message := fmt.Sprintf("⏰ *Upcoming Expirations* (within %s):\n", utils.FormatDuration(window))

	for _, tag := range tags {
		timeLeft := tag.ExpiresAt.Sub(time.Now())
		message += fmt.Sprintf("• <@%s> - *%s/%s* (expires in %s)\n",
			tag.AssignedTo, tag.Environment, tag.Name, utils.FormatDuration(timeLeft))
	}

	_, _, err := ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(message, false))
	if err != nil {
		return fmt.Errorf("failed to send upcoming expirations message: %w", err)
	}

	return nil
}

// CreateEnvironmentList creates a formatted list of environments and tags
func (ns *NotificationService) CreateEnvironmentList(environments map[string]*models.Environment) string {
	result := "*Available Environments and Tags:*\n\n"

	for envName, env := range environments {
		result += fmt.Sprintf("*%s:*\n", envName)
		for tagName, tag := range env.Tags {
			status := "🟢"
			statusText := tag.Status

			switch tag.Status {
			case "occupied":
				status = "🔴"
			case "maintenance":
				status = "🟡"
			}

			result += fmt.Sprintf("  %s `%s` (%s)", status, tagName, statusText)

			if tag.AssignedTo != "" {
				result += fmt.Sprintf(" - <@%s>", tag.AssignedTo)
				if !tag.ExpiresAt.IsZero() {
					timeLeft := tag.ExpiresAt.Sub(time.Now())
					if timeLeft > 0 {
						result += fmt.Sprintf(" (expires in %s)", utils.FormatDuration(timeLeft))
					} else {
						result += " (expired)"
					}
				}
			}
			result += "\n"
		}
		result += "\n"
	}

	return result
}

// CreatePositionInfo creates formatted position information for a user
func (ns *NotificationService) CreatePositionInfo(userPos *models.UserPosition, availableTags int) string {
	if userPos.Position == -1 {
		// User has an active assignment
		if userPos.ActiveAssignment != nil {
			timeLeft := userPos.ActiveAssignment.ExpiresAt.Sub(time.Now())
			if timeLeft > 0 {
				return fmt.Sprintf("🎉 You have an active assignment to *%s* in *%s* (expires in %s)",
					userPos.ActiveAssignment.Name, userPos.ActiveAssignment.Environment, utils.FormatDuration(timeLeft))
			} else {
				return "⚠️ Your assignment has expired and will be released soon"
			}
		}
		return "🎉 You have an active assignment"
	}

	if userPos.Position == 0 {
		return "❌ You are not in the queue"
	}

	// Get queue status for total count
	totalInQueue := 0
	if ns.queueService != nil {
		status := ns.queueService.GetQueueStatus()
		totalInQueue = status.TotalUsers
	}

	positionText := fmt.Sprintf("📋 You are *position %d*", userPos.Position)
	if totalInQueue > 0 {
		positionText += fmt.Sprintf(" of %d", totalInQueue)
	}
	positionText += " in the queue"

	waitText := ""
	if userPos.Position > 1 {
		waitText = fmt.Sprintf("\n⏱️ Estimated wait time: *%s*", utils.FormatDuration(userPos.EstimatedWait))
	} else {
		waitText = "\n🎯 You're next in line!"
	}

	requestText := ""
	if userPos.QueueItem != nil {
		if userPos.QueueItem.Tag != "" {
			requestText = fmt.Sprintf("\n🎯 Waiting for: *%s/%s* for *%s*",
				userPos.QueueItem.Environment, userPos.QueueItem.Tag, utils.FormatDuration(userPos.QueueItem.Duration))
		} else {
			requestText = fmt.Sprintf("\n🎯 Waiting for: *%s* for *%s*",
				userPos.QueueItem.Environment, utils.FormatDuration(userPos.QueueItem.Duration))
		}
	}

	availableText := fmt.Sprintf("\n🟢 Available tags: *%d*", availableTags)

	return positionText + waitText + requestText + availableText
}
