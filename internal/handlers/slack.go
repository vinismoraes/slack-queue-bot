package handlers

import (
	"fmt"
	"log"
	"strings"

	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
	"slack-queue-bot/internal/utils"

	"github.com/slack-go/slack"
)

// SlackHandler handles Slack-specific event processing
type SlackHandler struct {
	slackClient     *slack.Client
	queueService    interfaces.QueueService
	tagService      interfaces.TagService
	notificationSvc interfaces.NotificationService
	validationSvc   interfaces.ValidationService
	config          *models.ConfigSettings
}

// NewSlackHandler creates a new Slack event handler
func NewSlackHandler(
	slackClient *slack.Client,
	queueService interfaces.QueueService,
	tagService interfaces.TagService,
	notificationSvc interfaces.NotificationService,
	validationSvc interfaces.ValidationService,
	config *models.ConfigSettings,
) interfaces.SlackHandler {
	return &SlackHandler{
		slackClient:     slackClient,
		queueService:    queueService,
		tagService:      tagService,
		notificationSvc: notificationSvc,
		validationSvc:   validationSvc,
		config:          config,
	}
}

// HandleAppMention processes app mention events from Slack
func (sh *SlackHandler) HandleAppMention(event *models.SlackEvent) (*models.CommandResponse, error) {
	log.Printf("SlackHandler.HandleAppMention: userID=%s, text=%s", event.UserID, event.Text)

	// Get user information
	user, err := sh.slackClient.GetUserInfo(event.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}

	// Parse the command from the mention text
	cmd, err := sh.ParseCommand(event.Text, event.UserID, user.Name, event.ChannelID)
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	// Process the command
	response, err := sh.processCommand(cmd)
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	return response, nil
}

// HandleInteraction processes interactive events from Slack (buttons, etc.)
func (sh *SlackHandler) HandleInteraction(event *models.SlackEvent) (*models.CommandResponse, error) {
	log.Printf("SlackHandler.HandleInteraction: userID=%s, type=%s", event.UserID, event.Type)

	// join_queue is handled directly in main.go with proper modal support
	// Don't handle it here to avoid conflicts
	if event.Type == "join_queue" {
		return &models.CommandResponse{
			Success: false,
			Error:   "join_queue should be handled by main application, not SlackHandler",
		}, fmt.Errorf("join_queue handled by main app")
	}

	// For other interactions, handle as simple command mapping
	var cmd *models.CommandRequest

	switch event.Type {
	case "leave_queue":
		cmd = &models.CommandRequest{
			Command:   "leave",
			UserID:    event.UserID,
			Username:  event.UserID,
			ChannelID: event.ChannelID,
		}
	case "check_position":
		cmd = &models.CommandRequest{
			Command:   "position",
			UserID:    event.UserID,
			Username:  event.UserID,
			ChannelID: event.ChannelID,
		}
	case "list_environments":
		cmd = &models.CommandRequest{
			Command:   "list",
			UserID:    event.UserID,
			Username:  event.UserID,
			ChannelID: event.ChannelID,
		}
	case "assign_next":
		cmd = &models.CommandRequest{
			Command:   "assign",
			UserID:    event.UserID,
			Username:  event.UserID,
			ChannelID: event.ChannelID,
		}
	default:
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("unknown interaction type: %s", event.Type),
		}, nil
	}

	return sh.processCommand(cmd)
}

// ParseCommand parses a command from Slack text
func (sh *SlackHandler) ParseCommand(text, userID, username, channelID string) (*models.CommandRequest, error) {
	log.Printf("SlackHandler.ParseCommand: text=%s", text)

	// Extract bot user ID from the mention and remove it
	botUserIDStart := strings.Index(text, "<@")
	botUserIDEnd := strings.Index(text, ">")
	if botUserIDStart != -1 && botUserIDEnd != -1 {
		text = strings.TrimSpace(text[botUserIDEnd+1:])
	}

	// Split into parts
	parts := strings.Fields(text)
	if len(parts) == 0 {
		parts = []string{"help"}
	}

	cmd := &models.CommandRequest{
		Command:   parts[0],
		UserID:    userID,
		Username:  username,
		ChannelID: channelID,
		Arguments: parts[1:],
	}

	// Parse command-specific arguments
	switch parts[0] {
	case "join":
		return sh.parseJoinCommand(cmd, parts)
	case "release", "extend":
		return sh.parseTagCommand(cmd, parts)
	case "leave", "position", "status", "list", "assign", "help":
		// These commands don't need additional parsing
		return cmd, nil
	default:
		// Unknown command, treat as help
		cmd.Command = "help"
		return cmd, nil
	}
}

// parseJoinCommand parses join command arguments
func (sh *SlackHandler) parseJoinCommand(cmd *models.CommandRequest, parts []string) (*models.CommandRequest, error) {
	// join [environment] [tag] [duration]
	// join [environment] [duration]

	if len(parts) < 2 {
		return nil, fmt.Errorf("join command requires at least an environment")
	}

	cmd.Environment = parts[1]
	cmd.Duration = sh.config.DefaultDuration.ToDuration() // Default duration

	if len(parts) > 2 {
		// Could be tag or duration
		if len(parts) > 3 {
			// Has both tag and duration
			cmd.Tag = parts[2]
			durationStr := parts[3]
			duration, err := utils.ParseDuration(durationStr)
			if err != nil {
				return nil, fmt.Errorf("invalid duration format: %s", durationStr)
			}
			cmd.Duration = duration
		} else {
			// Only one additional argument - could be tag or duration
			arg := parts[2]

			// Try to parse as duration first
			if duration, err := utils.ParseDuration(arg); err == nil {
				cmd.Duration = duration
			} else {
				// Treat as tag name
				cmd.Tag = arg
			}
		}
	}

	return cmd, nil
}

// parseTagCommand parses commands that require environment and tag
func (sh *SlackHandler) parseTagCommand(cmd *models.CommandRequest, parts []string) (*models.CommandRequest, error) {
	if len(parts) < 3 {
		return nil, fmt.Errorf("%s command requires environment and tag", cmd.Command)
	}

	cmd.Environment = parts[1]
	cmd.Tag = parts[2]

	return cmd, nil
}

// processCommand processes a parsed command and returns a response
func (sh *SlackHandler) processCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	log.Printf("SlackHandler.processCommand: command=%s, userID=%s", cmd.Command, cmd.UserID)

	// Validate command structure
	if err := sh.validationSvc.ValidateCommand(cmd); err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	switch cmd.Command {
	case "join":
		return sh.handleJoinCommand(cmd)
	case "leave":
		return sh.handleLeaveCommand(cmd)
	case "position":
		return sh.handlePositionCommand(cmd)
	case "release":
		return sh.handleReleaseCommand(cmd)
	case "extend":
		return sh.handleExtendCommand(cmd)
	case "status":
		return sh.handleStatusCommand(cmd)
	case "assign":
		return sh.handleAssignCommand(cmd)
	case "list":
		return sh.handleListCommand(cmd)
	case "help":
		return sh.handleHelpCommand(cmd)
	default:
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("unknown command: %s", cmd.Command),
		}, nil
	}
}

// handleJoinCommand processes join queue requests
func (sh *SlackHandler) handleJoinCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	err := sh.queueService.JoinQueue(cmd.UserID, cmd.Username, cmd.Environment, cmd.Tag, cmd.Duration)
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	var message string
	if cmd.Tag != "" {
		message = fmt.Sprintf("✅ Added to queue for %s/%s for %s!",
			cmd.Environment, cmd.Tag, utils.FormatDuration(cmd.Duration))
	} else {
		message = fmt.Sprintf("✅ Added to queue for %s for %s!",
			cmd.Environment, utils.FormatDuration(cmd.Duration))
	}

	return &models.CommandResponse{
		Success:      true,
		Message:      message,
		ShouldNotify: true,
	}, nil
}

// handleLeaveCommand processes leave queue requests
func (sh *SlackHandler) handleLeaveCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	err := sh.queueService.LeaveQueue(cmd.UserID)
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	return &models.CommandResponse{
		Success:      true,
		Message:      "✅ Removed from queue!",
		ShouldNotify: true,
	}, nil
}

// handlePositionCommand processes position check requests
func (sh *SlackHandler) handlePositionCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	userPos := sh.queueService.GetUserPosition(cmd.UserID)
	status := sh.queueService.GetQueueStatus()

	message := sh.notificationSvc.CreatePositionInfo(userPos, status.AvailableTags)

	return &models.CommandResponse{
		Success: true,
		Message: message,
	}, nil
}

// handleReleaseCommand processes tag release requests
func (sh *SlackHandler) handleReleaseCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	err := sh.tagService.ReleaseTag(cmd.UserID, cmd.Environment, cmd.Tag)
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	// Send notification
	err = sh.notificationSvc.NotifyTagRelease(cmd.UserID, cmd.Environment, cmd.Tag)
	if err != nil {
		log.Printf("Warning: Failed to send release notification: %v", err)
	}

	return &models.CommandResponse{
		Success:      true,
		Message:      fmt.Sprintf("✅ Released %s/%s!", cmd.Environment, cmd.Tag),
		ShouldNotify: true,
	}, nil
}

// handleExtendCommand processes assignment extension requests
func (sh *SlackHandler) handleExtendCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	extension := sh.config.ExtensionTime
	err := sh.tagService.ExtendAssignment(cmd.UserID, cmd.Environment, cmd.Tag, extension.ToDuration())
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	// Get updated tag info for notification
	tag, err := sh.tagService.GetUserAssignment(cmd.UserID)
	if err == nil {
		err = sh.notificationSvc.NotifyAssignmentExtended(cmd.UserID, tag, cmd.Environment, extension.ToDuration())
		if err != nil {
			log.Printf("Warning: Failed to send extension notification: %v", err)
		}
	}

	return &models.CommandResponse{
		Success:      true,
		Message:      fmt.Sprintf("✅ Extended %s/%s by %s!", cmd.Environment, cmd.Tag, utils.FormatDuration(extension.ToDuration())),
		ShouldNotify: true,
	}, nil
}

// handleStatusCommand processes status display requests
func (sh *SlackHandler) handleStatusCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	// Return success with ephemeral flag - main.go will handle getting and sending the blocks
	return &models.CommandResponse{
		Success:     true,
		Message:     "status", // main.go will recognize this and send status blocks
		IsEphemeral: true,     // Send as ephemeral (private) message
	}, nil
}

// handleAssignCommand processes manual assignment requests
func (sh *SlackHandler) handleAssignCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	err := sh.queueService.ProcessQueue()
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	return &models.CommandResponse{
		Success:      true,
		Message:      "✅ Processed queue assignments!",
		ShouldNotify: true,
	}, nil
}

// handleListCommand processes environment list requests
func (sh *SlackHandler) handleListCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	status := sh.queueService.GetQueueStatus()
	message := sh.notificationSvc.CreateEnvironmentList(status.Environments)

	return &models.CommandResponse{
		Success: true,
		Message: message,
	}, nil
}

// handleHelpCommand processes help requests
func (sh *SlackHandler) handleHelpCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	helpText := "*Available Commands:*\n\n" +
		"*Queue Management:*\n" +
		"• `@bot join [environment] [tag] [duration]` - Join queue for specific tag with duration\n" +
		"• `@bot join [environment] [duration]` - Join queue for any tag in environment\n" +
		"• `@bot leave` - Leave the queue (if waiting)\n" +
		"• `@bot position` - Check your current queue position and estimated wait time\n\n" +
		"*Tag Management:*\n" +
		"• `@bot release [environment] [tag]` - Release your tag early (if assigned)\n" +
		"• `@bot extend [environment] [tag]` - Extend your assignment by " + utils.FormatDuration(sh.config.ExtensionTime.ToDuration()) + "\n\n" +
		"*Information:*\n" +
		"• `@bot status` - Show current queue and environment status\n" +
		"• `@bot list` - List all environments and available tags\n" +
		"• `@bot assign` - Manually assign next user in queue (admin)\n\n" +
		"*Duration Options* (30-minute intervals):\n" +
		"• `30m` = 30 minutes\n" +
		"• `1h` = 1 hour\n" +
		"• `1h30m` = 1 hour 30 minutes\n" +
		"• `2h` = 2 hours\n" +
		"• `2h30m` = 2 hours 30 minutes\n" +
		"• `3h` = 3 hours\n" +
		"• Default: " + utils.FormatDuration(sh.config.DefaultDuration.ToDuration()) + " (if no duration specified)\n\n" +
		"*Examples:*\n" +
		"• `@bot join cigna-test api 2h`\n" +
		"• `@bot join test1-au 1h30m`\n" +
		"• `@bot position`\n" +
		"• `@bot release test1-au api`\n" +
		"• `@bot extend test1-au api`\n" +
		"• `@bot list`"

	return &models.CommandResponse{
		Success: true,
		Message: helpText,
	}, nil
}

// CreateQueueStatusBlocks creates Slack blocks for queue status (implements interface)
func (sh *SlackHandler) CreateQueueStatusBlocks() (interface{}, error) {
	// This would return the actual Slack blocks, but for now we'll delegate to notification service
	err := sh.notificationSvc.BroadcastQueueUpdate()
	return nil, err
}
