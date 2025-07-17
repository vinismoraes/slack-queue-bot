package queue

import (
	"fmt"
	"log"
	"strings"
	"time"

	"slack-queue-bot/internal/database"

	"github.com/slack-go/slack"
)

// Manager handles queue operations using SQLite
type Manager struct {
	db          *database.DB
	slackClient *slack.Client
	channelID   string
}

// NewManager creates a new SQLite-based queue manager
func NewManager(db *database.DB, slackClient *slack.Client, channelID string) *Manager {
	return &Manager{
		db:          db,
		slackClient: slackClient,
		channelID:   channelID,
	}
}

// JoinQueue adds a user to the queue for a specific tag
func (m *Manager) JoinQueue(userID, username, environment, tag string, duration time.Duration) error {
	log.Printf("JoinQueue called: userID=%s, username=%s, env=%s, tag=%s, duration=%v", userID, username, environment, tag, duration)

	// Validate duration (minimum 30 minutes, maximum 3 hours, 30-minute intervals)
	if duration < 30*time.Minute {
		m.notifyUserBlock(userID, "⚠️", "Duration Too Short", "Minimum duration is 30 minutes.", "Please specify a longer duration.")
		return fmt.Errorf("duration too short")
	}
	if duration > 3*time.Hour {
		m.notifyUserBlock(userID, "⚠️", "Duration Too Long", "Maximum duration is 3 hours.", "Please specify a shorter duration.")
		return fmt.Errorf("duration too long")
	}

	// Check if duration is in 30-minute intervals
	if duration%(30*time.Minute) != 0 {
		m.notifyUserBlock(userID, "⚠️", "Invalid Duration", "Duration must be in 30-minute intervals.", "Valid options: 30m, 1h, 1h30m, 2h, 2h30m, 3h")
		return fmt.Errorf("invalid duration interval")
	}

	// Check if environment and tag exist
	environments, err := m.db.GetEnvironments()
	if err != nil {
		return fmt.Errorf("failed to get environments: %w", err)
	}

	var envFound bool
	var tagFound bool
	var tagIsAvailable bool
	for _, env := range environments {
		if env.Name == environment {
			envFound = true
			for _, t := range env.Tags {
				if t.Name == tag {
					tagFound = true
					if t.Status == "available" {
						tagIsAvailable = true
					}
					break
				}
			}
			break
		}
	}

	if !envFound {
		m.notifyUserBlock(userID, "❌", "Environment Not Found", fmt.Sprintf("Environment '%s' does not exist.", environment), "Use `@Queue Bot list` to see available environments.")
		return fmt.Errorf("environment not found")
	}

	if !tagFound {
		m.notifyUserBlock(userID, "❌", "Tag Not Found", fmt.Sprintf("Tag '%s' in environment '%s' does not exist.", tag, environment), "Use `@Queue Bot list` to see available tags.")
		return fmt.Errorf("tag not found")
	}

	if !tagIsAvailable {
		// Tag is occupied or in maintenance, add to queue normally
		log.Printf("Tag %s/%s is not available, adding user to queue", environment, tag)
		durationMinutes := int(duration.Minutes())
		err = m.db.JoinQueue(userID, username, environment, tag, durationMinutes)
		if err != nil {
			if err.Error() == "user is already in queue" {
				m.notifyUserBlock(userID, "ℹ️", "Already in Queue", "You are already in the queue.", "Use `@Queue Bot position` to check your spot or `@Queue Bot leave` to exit.")
			}
			return err
		}

		log.Printf("User %s added to queue successfully", userID)

		// Get user's position and notify them
		position, item, err := m.db.GetUserQueuePosition(userID)
		if err != nil {
			log.Printf("Error getting user position: %v", err)
		} else if item != nil {
			positionInfo := m.formatQueuePositionInfo(position, item)
			m.notifyUser(userID, positionInfo)
		}

		// Broadcast queue update
		m.BroadcastQueueUpdate()
		return nil
	}

	// Tag is available - check if there's anyone in queue for this tag
	isQueueEmpty, err := m.db.IsQueueEmptyForTag(environment, tag)
	if err != nil {
		log.Printf("Error checking queue for tag %s/%s: %v", environment, tag, err)
		// Fall back to normal queue behavior
		durationMinutes := int(duration.Minutes())
		err = m.db.JoinQueue(userID, username, environment, tag, durationMinutes)
		if err != nil {
			if err.Error() == "user is already in queue" {
				m.notifyUserBlock(userID, "ℹ️", "Already in Queue", "You are already in the queue.", "Use `@Queue Bot position` to check your spot or `@Queue Bot leave` to exit.")
			}
			return err
		}

		log.Printf("User %s added to queue successfully (fallback)", userID)
		m.BroadcastQueueUpdate()
		return nil
	}

	if isQueueEmpty {
		// No one in queue for this available tag - assign immediately!
		log.Printf("Tag %s/%s is available and queue is empty, assigning immediately to user %s", environment, tag, userID)

		expiresAt := time.Now().Add(duration)
		err = m.db.UpdateTagStatus(environment, tag, "occupied", &userID, &expiresAt)
		if err != nil {
			log.Printf("Error assigning tag immediately: %v", err)
			return fmt.Errorf("failed to assign tag: %w", err)
		}

		// Notify user of immediate assignment
		m.notifyUserBlock(userID, "🎉", "Tag Assigned!",
			fmt.Sprintf("You've been immediately assigned to *%s/%s* for *%s*!", environment, tag, formatDuration(duration)),
			fmt.Sprintf("Your assignment expires at %s", expiresAt.Format("15:04")))

		// Broadcast update to show the tag is now occupied
		m.BroadcastQueueUpdate()

		log.Printf("Successfully assigned tag %s/%s to user %s immediately", environment, tag, userID)
		return nil
	}

	// There are people in queue for this tag, add to queue normally
	log.Printf("Tag %s/%s is available but queue is not empty, adding user to queue", environment, tag)
	durationMinutes := int(duration.Minutes())
	err = m.db.JoinQueue(userID, username, environment, tag, durationMinutes)
	if err != nil {
		if err.Error() == "user is already in queue" {
			m.notifyUserBlock(userID, "ℹ️", "Already in Queue", "You are already in the queue.", "Use `@Queue Bot position` to check your spot or `@Queue Bot leave` to exit.")
		}
		return err
	}

	log.Printf("User %s added to queue successfully", userID)

	// Get user's position and notify them
	position, item, err := m.db.GetUserQueuePosition(userID)
	if err != nil {
		log.Printf("Error getting user position: %v", err)
	} else if item != nil {
		positionInfo := m.formatQueuePositionInfo(position, item)
		m.notifyUser(userID, positionInfo)
	}

	// Broadcast queue update
	m.BroadcastQueueUpdate()

	return nil
}

// LeaveQueue removes a user from the queue
func (m *Manager) LeaveQueue(userID string) error {
	// Get user info before removing them to provide better notifications
	position, item, err := m.db.GetUserQueuePosition(userID)
	if err != nil {
		return err
	}

	if position == 0 {
		return fmt.Errorf("user not in queue")
	}

	// Remove from database
	err = m.db.LeaveQueue(userID)
	if err != nil {
		return err
	}

	// Notify user that they left the queue
	if item != nil {
		m.notifyUserBlock(userID, "✅", "Left Queue",
			fmt.Sprintf("You've been removed from the queue for *%s/%s*.", item.Environment, item.Tag),
			"Join again anytime using the 🎯 Join Queue button!")
	}

	// Post public message to channel
	if item != nil {
		message := fmt.Sprintf("📤 <@%s> left the queue for *%s/%s*", userID, item.Environment, item.Tag)
		m.slackClient.PostMessage(m.channelID, slack.MsgOptionText(message, false))
	}

	// After someone leaves the queue, process it to assign available tags
	go m.ProcessQueue()

	// Broadcast queue update
	m.BroadcastQueueUpdate()

	return nil
}

// removeFromQueueSilently removes a user from queue without notifications (used during automatic assignment)
func (m *Manager) removeFromQueueSilently(userID string) error {
	return m.db.LeaveQueue(userID)
}

// GetUserQueuePosition returns a user's position in the queue
func (m *Manager) GetUserQueuePosition(userID string) (int, *database.QueueItem, error) {
	return m.db.GetUserQueuePosition(userID)
}

// GetQueuePositionInfo returns detailed information about a user's queue position
func (m *Manager) GetQueuePositionInfo(userID string) string {
	// Check if user has an active assignment
	hasAssignment, tag, err := m.db.HasUserActiveAssignment(userID)
	if err != nil {
		log.Printf("Error checking user assignment: %v", err)
		return "❌ Error checking your status"
	}

	if hasAssignment && tag != nil {
		if tag.ExpiresAt != nil {
			timeLeft := tag.ExpiresAt.Sub(time.Now())
			if timeLeft > 0 {
				return fmt.Sprintf("🎉 You have an active assignment to *%s* in *%s* (expires in %s)",
					tag.Name, tag.Environment, formatDuration(timeLeft))
			} else {
				return "⚠️ Your assignment has expired and will be released soon"
			}
		}
		return fmt.Sprintf("🎉 You have an active assignment to *%s* in *%s*", tag.Name, tag.Environment)
	}

	// Check queue position
	position, item, err := m.db.GetUserQueuePosition(userID)
	if err != nil {
		log.Printf("Error getting user queue position: %v", err)
		return "❌ Error checking your queue position"
	}

	if position == 0 {
		return "❌ You are not in the queue"
	}

	return m.formatQueuePositionInfo(position, item)
}

// formatQueuePositionInfo formats queue position information
func (m *Manager) formatQueuePositionInfo(position int, item *database.QueueItem) string {
	queue, err := m.db.GetQueue()
	if err != nil {
		log.Printf("Error getting queue: %v", err)
		return fmt.Sprintf("📋 You are position %d in the queue", position)
	}

	totalInQueue := len(queue)

	// Calculate estimated wait time (rough estimate: 30 minutes per person ahead)
	estimatedWaitMinutes := (position - 1) * 30
	estimatedWait := time.Duration(estimatedWaitMinutes) * time.Minute

	// Get available tags count
	availableTags, err := m.db.GetAvailableTags()
	if err != nil {
		log.Printf("Error getting available tags: %v", err)
	}

	positionText := fmt.Sprintf("📋 You are *position %d* of %d in the queue", position, totalInQueue)

	if item != nil {
		waitText := ""
		if position > 1 {
			waitText = fmt.Sprintf("\n⏱️ Estimated wait time: *%s*", formatDuration(estimatedWait))
		} else {
			waitText = "\n🎯 You're next in line!"
		}

		requestText := fmt.Sprintf("\n🎯 Waiting for: *%s/%s* for *%s*",
			item.Environment, item.Tag, formatDuration(time.Duration(item.DurationMinutes)*time.Minute))

		availableText := fmt.Sprintf("\n🟢 Available tags: *%d*", len(availableTags))

		return positionText + waitText + requestText + availableText
	}

	return positionText
}

// notifyUser sends a direct message to a user
func (m *Manager) notifyUser(userID, message string) {
	log.Printf("About to send DM to user %s: %s", userID, message)
	_, _, err := m.slackClient.PostMessage(userID, slack.MsgOptionText(message, false))
	if err != nil {
		log.Printf("Error notifying user %s: %v", userID, err)
	} else {
		log.Printf("Successfully sent DM to user %s", userID)
	}
}

// notifyUserBlock sends a block message to a user (for errors/info)
func (m *Manager) notifyUserBlock(userID, emoji, summary, detail, suggestion string) {
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("%s *%s*\n%s", emoji, summary, detail), false, false),
			nil, nil,
		),
	}
	if suggestion != "" {
		blocks = append(blocks, slack.NewContextBlock("", slack.NewTextBlockObject("mrkdwn", suggestion, false, false)))
	}
	_, _, err := m.slackClient.PostMessage(userID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		log.Printf("Error sending block message to user %s: %v", userID, err)
	}
}

// BroadcastQueueUpdate sends an update to the channel about current queue status
func (m *Manager) BroadcastQueueUpdate() {
	queue, err := m.db.GetQueue()
	if err != nil {
		log.Printf("Error getting queue for broadcast: %v", err)
		return
	}

	environments, err := m.db.GetEnvironments()
	if err != nil {
		log.Printf("Error getting environments for broadcast: %v", err)
		return
	}

	// Build blocks step by step with proper API calls
	var blocks []slack.Block

	// Header block
	blocks = append(blocks, slack.NewHeaderBlock(
		slack.NewTextBlockObject("plain_text", "📋 Test Environment Queue", false, false),
	))

	// Queue status section
	var queueText string
	if len(queue) == 0 {
		queueText = "*Queue:* Empty 😴"
	} else {
		queueText = fmt.Sprintf("*Queue:* %d users waiting\n", len(queue))
		for i, item := range queue {
			if i < 3 { // Show only first 3 to keep it concise
				queueText += fmt.Sprintf("  %d. <@%s>", i+1, item.UserID)
				if item.Environment != "" && item.Tag != "" {
					queueText += fmt.Sprintf(" (waiting for %s/%s)", item.Environment, item.Tag)
				}
				queueText += "\n"
			}
		}
		if len(queue) > 3 {
			queueText += fmt.Sprintf("  ... and %d more\n", len(queue)-3)
		}
	}

	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", queueText, false, false),
		nil, nil,
	))

	// Environment summary
	totalAvailable := 0
	totalOccupied := 0
	for _, env := range environments {
		for _, tag := range env.Tags {
			switch tag.Status {
			case "available":
				totalAvailable++
			case "occupied":
				totalOccupied++
			}
		}
	}

	summaryText := fmt.Sprintf("*Tags Summary:* 🟢 %d available • 🔴 %d occupied\n*Environments:* %d configured",
		totalAvailable, totalOccupied, len(environments))

	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", summaryText, false, false),
		nil, nil,
	))

	// Add divider before buttons
	blocks = append(blocks, slack.NewDividerBlock())

	// Add action buttons
	joinButton := slack.NewButtonBlockElement("join_queue", "join_queue", slack.NewTextBlockObject("plain_text", "🎯 Join Queue", true, false))
	joinButton.Style = "primary"

	leaveButton := slack.NewButtonBlockElement("leave_queue", "leave_queue", slack.NewTextBlockObject("plain_text", "🚪 Leave Queue", true, false))

	actionBlock := slack.NewActionBlock("queue_actions", joinButton, leaveButton)
	blocks = append(blocks, actionBlock)

	// Help text
	helpText := "*💡 Quick Commands:*\n• `@Queue Bot list` - See all environments\n• `@Queue Bot join <env> <tag1,tag2,...> [duration]` - Join queue\n• `@Queue Bot position` - Check your position"
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", helpText, false, false),
		nil, nil,
	))

	// Send the message
	_, _, err = m.slackClient.PostMessage(m.channelID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		log.Printf("Error posting message: %v", err)
	}
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

// GetAllEnvironments returns all environments for modal display
func (m *Manager) GetAllEnvironments() ([]database.Environment, error) {
	return m.db.GetEnvironments()
}

// GetEnvironmentTags returns all tags for a specific environment
func (m *Manager) GetEnvironmentTags(envName string) ([]database.Tag, error) {
	return m.db.GetTagsByEnvironment(envName)
}

// ProcessQueue processes the queue and assigns available tags to next users in line
func (m *Manager) ProcessQueue() error {
	log.Printf("Processing queue for automatic assignments...")

	// Get all available tags
	availableTags, err := m.db.GetAvailableTags()
	if err != nil {
		log.Printf("Error getting available tags: %v", err)
		return err
	}

	if len(availableTags) == 0 {
		log.Printf("No available tags to assign")
		return nil
	}

	// Get the queue
	queue, err := m.db.GetQueue()
	if err != nil {
		log.Printf("Error getting queue: %v", err)
		return err
	}

	if len(queue) == 0 {
		log.Printf("Queue is empty, nothing to process")
		return nil
	}

	var assignmentsMade int
	var assignedUsers []string

	// For each available tag, check if anyone in queue is waiting for it
	for _, tag := range availableTags {
		// Find the first person in queue waiting for this tag
		var nextUser *database.QueueItem
		for _, queueItem := range queue {
			if queueItem.Environment == tag.Environment && queueItem.Tag == tag.Name {
				nextUser = &queueItem
				break
			}
		}

		if nextUser != nil {
			log.Printf("Assigning tag %s/%s to user %s from queue", tag.Environment, tag.Name, nextUser.UserID)

			duration := time.Duration(nextUser.DurationMinutes) * time.Minute
			expiresAt := time.Now().Add(duration)

			// Assign the tag
			err = m.db.UpdateTagStatus(tag.Environment, tag.Name, "occupied", &nextUser.UserID, &expiresAt)
			if err != nil {
				log.Printf("Error assigning tag %s/%s to user %s: %v", tag.Environment, tag.Name, nextUser.UserID, err)
				continue
			}

			// Remove user from queue (use silent method to avoid duplicate notifications)
			err = m.removeFromQueueSilently(nextUser.UserID)
			if err != nil {
				log.Printf("Error removing user %s from queue: %v", nextUser.UserID, err)
				// Continue anyway, the assignment was successful
			}

			// Notify user of assignment
			m.notifyUserBlock(nextUser.UserID, "🎉", "Tag Assigned!",
				fmt.Sprintf("You've been assigned to *%s/%s* for *%s*!", tag.Environment, tag.Name, formatDuration(duration)),
				fmt.Sprintf("Your assignment expires at %s", expiresAt.Format("15:04")))

			// Post public message to channel about the assignment
			assignmentMsg := fmt.Sprintf("🎯 <@%s> has been automatically assigned to *%s/%s* for *%s* (expires at %s)",
				nextUser.UserID, tag.Environment, tag.Name, formatDuration(duration), expiresAt.Format("15:04"))
			m.slackClient.PostMessage(m.channelID, slack.MsgOptionText(assignmentMsg, false))

			assignmentsMade++
			assignedUsers = append(assignedUsers, fmt.Sprintf("<@%s> → %s/%s", nextUser.UserID, tag.Environment, tag.Name))
			log.Printf("Successfully assigned tag %s/%s to user %s", tag.Environment, tag.Name, nextUser.UserID)
		}
	}

	if assignmentsMade > 0 {
		log.Printf("Made %d automatic assignments: %v", assignmentsMade, assignedUsers)

		// Send a summary message to the channel if multiple assignments were made
		if assignmentsMade > 1 {
			summaryMsg := fmt.Sprintf("🔄 *%d automatic assignments made:*\n• %s",
				assignmentsMade, strings.Join(assignedUsers, "\n• "))
			m.slackClient.PostMessage(m.channelID, slack.MsgOptionText(summaryMsg, false))
		}

		// Broadcast update to show the new assignments
		m.BroadcastQueueUpdate()
	}

	return nil
}

// GetChannelID returns the channel ID for posting messages
func (m *Manager) GetChannelID() string {
	return m.channelID
}

// TTL-based Expiration Management

// ProcessTTLExpirations processes all expired events using the TTL system
func (m *Manager) ProcessTTLExpirations() error {
	log.Printf("Processing TTL-based expirations...")

	// Get all expired events
	expiredEvents, err := m.db.GetExpiredEvents()
	if err != nil {
		log.Printf("Error getting expired events: %v", err)
		return err
	}

	if len(expiredEvents) == 0 {
		log.Printf("No expired events found")
		return nil
	}

	var releasedTags []string
	var releasedUsers []string
	var processedEvents []int

	for _, event := range expiredEvents {
		timeOverdue := time.Now().Sub(event.ExpiresAt)
		log.Printf("Processing expiration event: tag %s/%s from user %s (expired at %v, %v overdue)",
			event.Environment, event.TagName, event.AssignedTo, event.ExpiresAt, timeOverdue)

		// Process the expiration event (releases tag and marks event as processed)
		err = m.db.ProcessExpirationEvent(event.ID)
		if err != nil {
			log.Printf("Error processing expiration event %d: %v", event.ID, err)
			continue
		}

		// Notify the user that their assignment has expired
		m.notifyUserBlock(event.AssignedTo, "⏰", "Assignment Expired",
			fmt.Sprintf("Your assignment to *%s/%s* has expired and been released.", event.Environment, event.TagName),
			"Join the queue again if you need more time.")

		releasedUsers = append(releasedUsers, fmt.Sprintf("<@%s>", event.AssignedTo))
		releasedTags = append(releasedTags, fmt.Sprintf("%s/%s", event.Environment, event.TagName))
		processedEvents = append(processedEvents, event.ID)
	}

	if len(releasedTags) > 0 {
		log.Printf("Processed %d TTL expirations: %v", len(releasedTags), releasedTags)

		// Notify the channel about the releases with user mentions
		if len(releasedTags) == 1 {
			releaseMsg := fmt.Sprintf("⏰ Assignment expired: *%s* (was assigned to %s)",
				releasedTags[0], releasedUsers[0])
			m.slackClient.PostMessage(m.channelID, slack.MsgOptionText(releaseMsg, false))
		} else {
			var tagUserPairs []string
			for i, tag := range releasedTags {
				if i < len(releasedUsers) {
					tagUserPairs = append(tagUserPairs, fmt.Sprintf("• %s (%s)", tag, releasedUsers[i]))
				} else {
					tagUserPairs = append(tagUserPairs, fmt.Sprintf("• %s", tag))
				}
			}
			releaseMsg := fmt.Sprintf("⏰ *%d assignments expired:*\n%s",
				len(releasedTags), strings.Join(tagUserPairs, "\n"))
			m.slackClient.PostMessage(m.channelID, slack.MsgOptionText(releaseMsg, false))
		}

		// Process the queue to assign newly available tags
		go m.ProcessQueue()

		// Broadcast update to show the changes
		m.BroadcastQueueUpdate()
	}

	return nil
}

// GetUpcomingExpirations returns assignments that will expire soon
func (m *Manager) GetUpcomingExpirations(within time.Duration) ([]database.ExpirationEvent, error) {
	return m.db.GetUpcomingExpirations(within)
}

// CleanupOldExpirationEvents removes old processed expiration events
func (m *Manager) CleanupOldExpirationEvents() error {
	// Clean up events older than 24 hours
	return m.db.CleanupProcessedEvents(24 * time.Hour)
}

// CheckImmediateExpirations checks for any assignments that have just expired and processes them immediately
func (m *Manager) CheckImmediateExpirations() error {
	// This method can be called when we want to check for expirations right now
	// without waiting for the periodic check
	return m.ProcessTTLExpirations()
}

// Legacy method maintained for compatibility - now delegates to TTL system
func (m *Manager) ReleaseExpiredAssignments() error {
	// For backward compatibility, delegate to the new TTL system
	return m.ProcessTTLExpirations()
}
