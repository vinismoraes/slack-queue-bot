package services

import (
	"fmt"
	"log"
	"strings"
	"time"

	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
	"slack-queue-bot/internal/utils"

	"github.com/slack-go/slack"
)

// NotificationService handles all Slack messaging operations
type NotificationService struct {
	slackClient       *slack.Client
	channelID         string
	queueService      interfaces.QueueService
	configService     interfaces.ConfigService
	config            *models.ConfigSettings
	lastStatusMessage string    // Track last status message timestamp for updates
	lastStatusTime    time.Time // Track when last status was sent
}

// NewNotificationService creates a new notification service
func NewNotificationService(
	slackClient *slack.Client,
	channelID string,
	configService interfaces.ConfigService,
	config *models.ConfigSettings,
) interfaces.NotificationService {
	return &NotificationService{
		slackClient:   slackClient,
		channelID:     channelID,
		configService: configService,
		config:        config,
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

// BroadcastOrUpdateQueueStatus updates the previous status message if it exists and is recent, otherwise posts a new one
func (ns *NotificationService) BroadcastOrUpdateQueueStatus() error {
	log.Printf("NotificationService.BroadcastOrUpdateQueueStatus")

	if ns.queueService == nil {
		return fmt.Errorf("queue service not set")
	}

	status := ns.queueService.GetQueueStatus()
	blocks := ns.createQueueStatusBlocks(status)

	// Check if we have a recent status message to update (within last 5 minutes)
	if ns.lastStatusMessage != "" && time.Since(ns.lastStatusTime) < 5*time.Minute {
		log.Printf("Attempting to update previous status message: %s", ns.lastStatusMessage)

		// Try to update the existing message
		_, _, _, err := ns.slackClient.UpdateMessage(ns.channelID, ns.lastStatusMessage, slack.MsgOptionBlocks(blocks...))
		if err != nil {
			log.Printf("Failed to update previous message (will post new one): %v", err)
			log.Printf("Error details: %T - %s", err, err.Error())
			// Fall back to posting new message
			return ns.postNewStatusMessage(blocks)
		}

		log.Printf("Successfully updated existing status message")
		// Update timestamp but keep same message ID
		ns.lastStatusTime = time.Now()
		return nil
	}

	// Post new message
	return ns.postNewStatusMessage(blocks)
}

// UpdateExistingQueueStatus updates only the existing status message, doesn't post new ones
// This is used for silent operations like tag releases that shouldn't spam the channel
func (ns *NotificationService) UpdateExistingQueueStatus() error {
	log.Printf("NotificationService.UpdateExistingQueueStatus")

	if ns.queueService == nil {
		return fmt.Errorf("queue service not set")
	}

	// Only update if we have a recent status message (within last 10 minutes)
	if ns.lastStatusMessage == "" || time.Since(ns.lastStatusTime) > 10*time.Minute {
		log.Printf("No recent status message to update (last: %v ago), skipping silent update", time.Since(ns.lastStatusTime))
		return nil
	}

	status := ns.queueService.GetQueueStatus()
	blocks := ns.createQueueStatusBlocks(status)

	// Try to update the existing message
	_, _, _, err := ns.slackClient.UpdateMessage(ns.channelID, ns.lastStatusMessage, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		log.Printf("Failed to update existing status message: %v", err)
		return err
	}

	log.Printf("Successfully updated existing status message silently")
	// Update timestamp but keep same message ID
	ns.lastStatusTime = time.Now()
	return nil
}

// postNewStatusMessage posts a new status message and tracks it
func (ns *NotificationService) postNewStatusMessage(blocks []slack.Block) error {
	_, timestamp, err := ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		return fmt.Errorf("failed to post new status message: %w", err)
	}

	// Track the new message for future updates
	ns.lastStatusMessage = timestamp
	ns.lastStatusTime = time.Now()
	log.Printf("Posted new status message: %s", timestamp)

	return nil
}

// TrackStatusMessage manually tracks a status message for future updates
func (ns *NotificationService) TrackStatusMessage(timestamp string) {
	ns.lastStatusMessage = timestamp
	ns.lastStatusTime = time.Now()
	log.Printf("Tracking status message for updates: %s", timestamp)
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

// CreateQueueStatusBlocksForUser creates and returns status blocks for a specific user
// This allows for user-specific button visibility (ephemeral responses)
func (ns *NotificationService) CreateQueueStatusBlocksForUser(userID string) ([]slack.Block, error) {
	log.Printf("NotificationService.CreateQueueStatusBlocksForUser: userID=%s", userID)

	if ns.queueService == nil {
		return nil, fmt.Errorf("queue service not set")
	}

	status := ns.queueService.GetQueueStatus()
	blocks := ns.createQueueStatusBlocksForUser(status, userID)

	return blocks, nil
}

// NotifyAssignment notifies a user of tag assignment
func (ns *NotificationService) NotifyAssignment(userID string, tag *models.Tag, environment string) error {
	log.Printf("NotificationService.NotifyAssignment: userID=%s, tag=%s/%s", userID, environment, tag.Name)

	expiryTime := tag.ExpiresAt.Format("15:04")

	// Send only channel notification - no DM to user
	channelMessage := fmt.Sprintf("🎉 <@%s> has been assigned to *%s/%s* (expires at %s)",
		userID, environment, tag.Name, expiryTime)

	_, _, err := ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(channelMessage, false))
	if err != nil {
		log.Printf("Warning: Failed to send assignment channel message: %v", err)
	}

	return err
}

// NotifyExpiration notifies about tag expiration
func (ns *NotificationService) NotifyExpiration(userID string, tag *models.Tag, environment string) error {
	log.Printf("NotificationService.NotifyExpiration: userID=%s, tag=%s/%s", userID, environment, tag.Name)

	// Send only channel notification - no DM to user
	channelMessage := fmt.Sprintf("⏰ <@%s>'s assignment to *%s/%s* has expired and been released",
		userID, environment, tag.Name)

	_, _, err := ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(channelMessage, false))
	if err != nil {
		log.Printf("Warning: Failed to send expiration channel message: %v", err)
	}

	return err
}

// NotifyQueueJoined notifies a user they've joined the queue
func (ns *NotificationService) NotifyQueueJoined(userID string, position int) error {
	log.Printf("NotificationService.NotifyQueueJoined: userID=%s, position=%d", userID, position)

	// No longer send DM notifications for queue joins - only channel notifications are sent from main.go
	return nil
}

// NotifyQueueLeft notifies a user they've left the queue
func (ns *NotificationService) NotifyQueueLeft(userID string) error {
	log.Printf("NotificationService.NotifyQueueLeft: userID=%s", userID)

	// Send only channel notification - no DM to user
	channelMessage := fmt.Sprintf("👋 <@%s> has left the queue", userID)

	_, _, err := ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(channelMessage, false))
	if err != nil {
		log.Printf("Warning: Failed to send queue left channel message: %v", err)
	}

	return err
}

// NotifyTagRelease notifies about manual tag release
func (ns *NotificationService) NotifyTagRelease(userID string, environment, tag string) error {
	log.Printf("NotificationService.NotifyTagRelease: userID=%s, tag=%s/%s", userID, environment, tag)

	// Send only channel notification - no DM to user
	channelMessage := fmt.Sprintf("✅ <@%s> has released *%s/%s*", userID, environment, tag)

	_, _, err := ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(channelMessage, false))
	if err != nil {
		log.Printf("Warning: Failed to send release channel message: %v", err)
	}

	return err
}

// NotifyAssignmentExtended notifies about assignment extension
func (ns *NotificationService) NotifyAssignmentExtended(userID string, tag *models.Tag, environment string, extension time.Duration) error {
	log.Printf("NotificationService.NotifyAssignmentExtended: userID=%s, tag=%s/%s, extension=%v", userID, environment, tag.Name, extension)

	newExpiryTime := tag.ExpiresAt.Format("15:04")
	extensionText := utils.FormatDuration(extension)

	// Send only channel notification - no DM to user
	channelMessage := fmt.Sprintf("⏰ <@%s> extended assignment to *%s/%s* by *%s* (expires at %s)",
		userID, environment, tag.Name, extensionText, newExpiryTime)

	_, _, err := ns.slackClient.PostMessage(ns.channelID, slack.MsgOptionText(channelMessage, false))
	if err != nil {
		log.Printf("Warning: Failed to send extension channel message: %v", err)
	}

	return err
}

// createQueueStatusBlocks creates Slack blocks for queue status display
func (ns *NotificationService) createQueueStatusBlocks(status *models.QueueStatus) []slack.Block {
	return ns.createQueueStatusBlocksForUser(status, "")
}

// createQueueStatusBlocksForUser creates Slack blocks for queue status display with user-specific buttons
func (ns *NotificationService) createQueueStatusBlocksForUser(status *models.QueueStatus, userID string) []slack.Block {
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

	summaryText := fmt.Sprintf("🟢 *%d* available • 🔵 *%d* in use • 👥 *%d* in queue",
		totalAvailable, totalOccupied, status.TotalUsers)

	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", summaryText, false, false),
		nil, nil,
	))

	// === CURRENT USER SECTION (if userID provided) ===
	if userID != "" {
		var userAssignments []*models.Tag
		var userQueueItems []models.QueueItem
		var otherUserAssignments []*models.Tag
		var otherQueueItems []models.QueueItem

		// Separate current user's data from others
		for _, env := range status.Environments {
			for _, tag := range env.Tags {
				if tag.Status == "occupied" {
					if tag.AssignedTo == userID {
						userAssignments = append(userAssignments, tag)
					} else {
						otherUserAssignments = append(otherUserAssignments, tag)
					}
				}
			}
		}

		for _, queueItem := range status.Queue {
			if queueItem.UserID == userID {
				userQueueItems = append(userQueueItems, queueItem)
			} else {
				otherQueueItems = append(otherQueueItems, queueItem)
			}
		}

		// Show current user's assignments first
		if len(userAssignments) > 0 {
			blocks = append(blocks, slack.NewDividerBlock())

			adminIndicator := ""
			if ns.configService != nil && ns.configService.IsAdmin(userID) {
				adminIndicator = " 👑"
			}

			assignmentText := fmt.Sprintf("*🎉 Your Active Assignments:%s*\n", adminIndicator)
			for _, tag := range userAssignments {
				timeLeft := ""
				if !tag.ExpiresAt.IsZero() {
					remaining := tag.ExpiresAt.Sub(time.Now())
					if remaining > 0 {
						timeLeft = fmt.Sprintf("\n   ⏰ expires in %s", utils.FormatDuration(remaining))
					} else {
						timeLeft = "\n   ⚠️ expired"
					}
				}
				assignmentText += fmt.Sprintf("• `%s/%s`%s\n", tag.Environment, tag.Name, timeLeft)
			}

			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", assignmentText, false, false),
				nil, nil,
			))
		}

		// Show current user's queue positions
		if len(userQueueItems) > 0 {
			if len(userAssignments) == 0 {
				blocks = append(blocks, slack.NewDividerBlock())
			}

			adminIndicator := ""
			if ns.configService != nil && ns.configService.IsAdmin(userID) {
				adminIndicator = " 👑"
			}

			queueText := fmt.Sprintf("*⏳ Your Queue Positions:%s*\n", adminIndicator)
			for _, item := range userQueueItems {
				// Find position in the overall queue for this environment/tag
				position := 1
				for _, otherItem := range status.Queue {
					if otherItem.Environment == item.Environment && otherItem.Tag == item.Tag {
						if otherItem.UserID == userID {
							break
						}
						position++
					}
				}

				queueText += fmt.Sprintf("• *Position %d* for %s/%s (duration: %s)\n",
					position, item.Environment, item.Tag, utils.FormatDuration(item.Duration))
			}

			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", queueText, false, false),
				nil, nil,
			))
		}

		// === OTHER USERS SUMMARY SECTION ===
		if len(otherUserAssignments) > 0 || len(otherQueueItems) > 0 {
			blocks = append(blocks, slack.NewDividerBlock())

			othersText := "*👥 Other Activity:*\n"

			// Group other assignments by environment
			envAssignments := make(map[string][]string)
			for _, tag := range otherUserAssignments {
				timeLeft := ""
				if !tag.ExpiresAt.IsZero() {
					remaining := tag.ExpiresAt.Sub(time.Now())
					if remaining > 0 {
						timeLeft = fmt.Sprintf(" (expires in %s)", utils.FormatDuration(remaining))
					} else {
						timeLeft = " (expired)"
					}
				}
				assignment := fmt.Sprintf("%s%s by <@%s>", tag.Name, timeLeft, tag.AssignedTo)
				envAssignments[tag.Environment] = append(envAssignments[tag.Environment], assignment)
			}

			// Show environment summaries
			for envName, assignments := range envAssignments {
				if len(assignments) <= 3 {
					othersText += fmt.Sprintf("• *%s:* %s\n", envName, strings.Join(assignments, ", "))
				} else {
					othersText += fmt.Sprintf("• *%s:* %s and %d more\n",
						envName, strings.Join(assignments[:3], ", "), len(assignments)-3)
				}
			}

			// Show queue summary with user information
			if len(otherQueueItems) > 0 {
				// Group by environment/tag and show users
				queueGroups := make(map[string][]string)
				for _, item := range otherQueueItems {
					key := fmt.Sprintf("%s/%s", item.Environment, item.Tag)
					userInfo := fmt.Sprintf("<@%s>", item.UserID)
					queueGroups[key] = append(queueGroups[key], userInfo)
				}

				var queueEntries []string
				for key, users := range queueGroups {
					if len(users) == 1 {
						queueEntries = append(queueEntries, fmt.Sprintf("%s by %s", key, users[0]))
					} else {
						queueEntries = append(queueEntries, fmt.Sprintf("%s by %s (%d waiting)", key, users[0], len(users)))
					}
				}

				if len(queueEntries) > 0 {
					if len(envAssignments) > 0 {
						othersText += "\n"
					}
					othersText += fmt.Sprintf("• *Queue:* %s\n", strings.Join(queueEntries, ", "))
				}
			}

			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", othersText, false, false),
				nil, nil,
			))
		}
	} else {
		// === GENERAL BROADCAST (no specific user) ===
		// Show a more compact view for general broadcasts
		if totalOccupied > 0 {
			blocks = append(blocks, slack.NewDividerBlock())

			// Group occupied tags by environment and user with better formatting
			envUserOccupied := make(map[string]map[string][]string)
			for _, env := range status.Environments {
				for _, tag := range env.Tags {
					if tag.Status == "occupied" {
						if envUserOccupied[env.Name] == nil {
							envUserOccupied[env.Name] = make(map[string][]string)
						}

						timeLeft := ""
						if !tag.ExpiresAt.IsZero() {
							remaining := tag.ExpiresAt.Sub(time.Now())
							if remaining > 0 {
								timeLeft = fmt.Sprintf(" ⏰ %s", utils.FormatDuration(remaining))
							} else {
								timeLeft = " ⚠️ expired"
							}
						}

						tagInfo := fmt.Sprintf("`%s`%s", tag.Name, timeLeft)
						envUserOccupied[env.Name][tag.AssignedTo] = append(envUserOccupied[env.Name][tag.AssignedTo], tagInfo)
					}
				}
			}

			occupiedText := "*🔵 Tags In Use:*\n"
			for envName, userTags := range envUserOccupied {
				occupiedText += fmt.Sprintf("• *%s:*\n", envName)
				for userID, tags := range userTags {
					if len(tags) == 1 {
						occupiedText += fmt.Sprintf("  <@%s>: %s\n", userID, tags[0])
					} else {
						occupiedText += fmt.Sprintf("  <@%s>: %s\n", userID, strings.Join(tags, ", "))
					}
				}
			}

			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", occupiedText, false, false),
				nil, nil,
			))
		}

		// Show queue summary with user information
		if len(status.Queue) > 0 {
			if totalOccupied > 0 {
				blocks = append(blocks, slack.NewDividerBlock())
			}

			queueText := "*👥 Queue:*\n"
			// Group by environment/tag and show users
			queueGroups := make(map[string][]string)
			for _, item := range status.Queue {
				key := fmt.Sprintf("%s/%s", item.Environment, item.Tag)
				userInfo := fmt.Sprintf("<@%s>", item.UserID)
				queueGroups[key] = append(queueGroups[key], userInfo)
			}

			var queueEntries []string
			for key, users := range queueGroups {
				if len(users) == 1 {
					queueEntries = append(queueEntries, fmt.Sprintf("%s by %s", key, users[0]))
				} else {
					queueEntries = append(queueEntries, fmt.Sprintf("%s by %s (%d waiting)", key, users[0], len(users)))
				}
			}

			queueText += fmt.Sprintf("• %s\n", strings.Join(queueEntries, ", "))

			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", queueText, false, false),
				nil, nil,
			))
		}
	}

	// Add divider before buttons
	blocks = append(blocks, slack.NewDividerBlock())

	// Interactive Buttons - contextual based on user status
	var actionElements []slack.BlockElement

	// Always show Join Queue button
	joinButton := slack.NewButtonBlockElement("join_queue", "join_queue",
		slack.NewTextBlockObject("plain_text", "🎯 Join Queue", true, false))
	joinButton.Style = "primary"
	actionElements = append(actionElements, joinButton)

	// Show admin-only buttons if user is admin
	isUserAdmin := false
	if userID != "" && ns.configService != nil {
		isUserAdmin = ns.configService.IsAdmin(userID)
	}

	// Determine user status for contextual buttons
	userInQueue := false
	userHasAssignments := false
	userAssignmentCount := 0
	userQueueCount := 0

	if userID != "" {
		// Check user's assignments and queue positions from the status
		for _, env := range status.Environments {
			for _, tag := range env.Tags {
				if tag.Status == "occupied" {
					if tag.AssignedTo == userID {
						userHasAssignments = true
						userAssignmentCount++
					}
				}
			}
		}

		for _, queueItem := range status.Queue {
			if queueItem.UserID == userID {
				userInQueue = true
				userQueueCount++
			}
		}
	}

	// Show contextual buttons based on user state

	if userHasAssignments {
		// User has assignments - show Release button(s)
		releaseText := "🔓 Release Tags"
		if userAssignmentCount == 1 {
			releaseText = "🔓 Release Tag"
		} else {
			releaseText = "🔓 Release All Tags"
		}
		releaseButton := slack.NewButtonBlockElement("release_tags", "release_tags",
			slack.NewTextBlockObject("plain_text", releaseText, true, false))
		releaseButton.Style = "danger"
		actionElements = append(actionElements, releaseButton)
	}

	if userInQueue {
		// User is in queue - show Leave button(s)
		leaveText := "🚪 Leave Queue"
		if userQueueCount > 1 {
			leaveText = "🚪 Leave All Queues"
		}
		leaveButton := slack.NewButtonBlockElement("leave_queue", "leave_queue",
			slack.NewTextBlockObject("plain_text", leaveText, true, false))
		leaveButton.Style = "danger"
		actionElements = append(actionElements, leaveButton)
	}

	// Add action buttons
	if len(actionElements) > 0 {
		blocks = append(blocks, slack.NewActionBlock("", actionElements...))
	}

	// Add admin-only actions if user is admin
	if isUserAdmin {
		adminElements := []slack.BlockElement{
			slack.NewButtonBlockElement("assign_next", "assign_next",
				slack.NewTextBlockObject("plain_text", "⚡ Assign Next", true, false)),
		}

		adminActionBlock := slack.NewActionBlock("admin_actions", adminElements...)
		blocks = append(blocks, adminActionBlock)

		// Add admin indicator context
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", "👑 _Admin controls visible_", false, false)))
	}

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
				status = "🔵"
			case "maintenance":
				status = "🟡"
			}

			result += fmt.Sprintf("  %s `%s` (%s)", status, tagName, statusText)

			if tag.AssignedTo != "" {
				result += fmt.Sprintf(" → <@%s>", tag.AssignedTo)
				if !tag.ExpiresAt.IsZero() {
					timeLeft := tag.ExpiresAt.Sub(time.Now())
					if timeLeft > 0 {
						result += fmt.Sprintf("\n     ⏰ expires in %s", utils.FormatDuration(timeLeft))
					} else {
						result += "\n     ⚠️ expired"
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
	if userPos.Position == -1 || len(userPos.ActiveAssignments) > 0 {
		// User has active assignments
		if len(userPos.ActiveAssignments) > 0 {
			var assignmentTexts []string
			for _, assignment := range userPos.ActiveAssignments {
				timeLeft := assignment.ExpiresAt.Sub(time.Now())
				if timeLeft > 0 {
					assignmentText := fmt.Sprintf("*%s* in *%s* (expires in %s)",
						assignment.Name, assignment.Environment, utils.FormatDuration(timeLeft))
					assignmentTexts = append(assignmentTexts, assignmentText)
				} else {
					assignmentText := fmt.Sprintf("*%s* in *%s* (expired)",
						assignment.Name, assignment.Environment)
					assignmentTexts = append(assignmentTexts, assignmentText)
				}
			}

			if len(assignmentTexts) == 1 {
				return fmt.Sprintf("🎉 You have an active assignment to %s", assignmentTexts[0])
			} else {
				return fmt.Sprintf("🎉 You have %d active assignments:\n• %s",
					len(assignmentTexts), strings.Join(assignmentTexts, "\n• "))
			}
		}
		return "🎉 You have active assignments"
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
