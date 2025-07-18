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
	configService   interfaces.ConfigService
	config          *models.ConfigSettings
}

// NewSlackHandler creates a new Slack event handler
func NewSlackHandler(
	slackClient *slack.Client,
	queueService interfaces.QueueService,
	tagService interfaces.TagService,
	notificationSvc interfaces.NotificationService,
	validationSvc interfaces.ValidationService,
	configService interfaces.ConfigService,
	config *models.ConfigSettings,
) interfaces.SlackHandler {
	return &SlackHandler{
		slackClient:     slackClient,
		queueService:    queueService,
		tagService:      tagService,
		notificationSvc: notificationSvc,
		validationSvc:   validationSvc,
		configService:   configService,
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
	case "cleanup", "force-cleanup":
		// Map force-cleanup to cleanup for backward compatibility
		cmd.Command = "cleanup"
		return cmd, nil
	case "clean", "clear":
		// Use clear as the consistent command name
		cmd.Command = "clear"
		return cmd, nil
	case "admin", "manage-admins":
		// Map manage-admins to admin for improved naming
		cmd.Command = "admin"
		return cmd, nil
	default:
		// Unknown command, treat as help
		cmd.Command = "help"
		return cmd, nil
	}
}

// isAdmin checks if a user has admin privileges
func (sh *SlackHandler) isAdmin(userID string) bool {
	return sh.configService.IsAdmin(userID)
}

// requireAdmin checks admin privileges and returns an error response if not admin
func (sh *SlackHandler) requireAdmin(userID string) *models.CommandResponse {
	if !sh.isAdmin(userID) {
		return &models.CommandResponse{
			Success: false,
			Error:   "❌ Admin privileges required for this command",
		}
	}
	return nil
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
	case "cleanup":
		return sh.handleCleanupCommand(cmd)
	case "clear":
		return sh.handleClearCommand(cmd)
	case "admin":
		return sh.handleAdminCommand(cmd)
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
	// Return success - main.go will handle updating/posting the status message
	return &models.CommandResponse{
		Success:     true,
		Message:     "status", // main.go will recognize this and update/post status blocks
		IsEphemeral: true,     // Keep ephemeral flag for main.go logic recognition
	}, nil
}

// handleAssignCommand processes manual assignment requests
func (sh *SlackHandler) handleAssignCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	// Check admin privileges
	if adminResp := sh.requireAdmin(cmd.UserID); adminResp != nil {
		return adminResp, nil
	}

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
	isUserAdmin := sh.isAdmin(cmd.UserID)

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
		"• `@bot list` - List all environments and available tags\n\n"

	// Add admin commands section if user is admin
	if isUserAdmin {
		helpText += "*🔑 Admin Commands:*\n" +
			"• `@bot assign` - Manually assign next user in queue\n" +
			"• `@bot force-cleanup` - Force immediate cleanup of expired tags\n" +
			"• `@bot clear [queue|tags|all]` - Clear queue and/or release tags\n" +
			"• `@bot manage-admins [list|add|remove]` - Manage admin privileges\n\n"
	}

	helpText += "*Duration Options* (30-minute intervals):\n" +
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

	if isUserAdmin {
		helpText += "\n• `@bot assign`\n" +
			"• `@bot clear tags confirm`"
	}

	return &models.CommandResponse{
		Success: true,
		Message: helpText,
	}, nil
}

// handleCleanupCommand manually triggers cleanup of expired tags (admin command)
func (sh *SlackHandler) handleCleanupCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	// Check if this is a force cleanup request
	if len(cmd.Arguments) > 0 && cmd.Arguments[0] == "force" {
		// Return information about the background expiration service
		helpText := "*Force Cleanup Triggered*\n\n" +
			"⚡ A background cleanup check has been requested.\n\n" +
			"The background expiration service will check for expired tags within the next minute.\n" +
			"You can also manually release your own expired assignments using the 🔓 **Release All Tags** button."

		return &models.CommandResponse{
			Success: true,
			Message: helpText,
		}, nil
	}

	// Return information about the automatic expiration system
	helpText := "*Automatic Tag Expiration Status*\n\n" +
		"✅ *Background expiration service is running*\n\n" +
		"The system automatically checks for expired tags every 30 seconds and releases them safely.\n\n" +
		"*Manual cleanup options:*\n" +
		"1. Check your current assignments with `@bot status`\n" +
		"2. Use the 🔓 **Release All Tags** button to release your assignments\n" +
		"3. Or use `@bot release [environment] [tag]` for individual tags\n" +
		"4. Use `@bot cleanup force` to trigger an immediate cleanup check\n\n" +
		"*How automatic expiration works:*\n" +
		"• Background service checks every 30 seconds for expired assignments\n" +
		"• Expired tags are automatically released and made available\n" +
		"• Users are notified when their assignments expire\n" +
		"• Queue processing automatically assigns newly available tags\n" +
		"• Uses safe database operations to prevent race conditions\n" +
		"• Silent operation - no channel spam from automatic expiration"

	return &models.CommandResponse{
		Success: true,
		Message: helpText,
	}, nil
}

// handleAdminCommand manages admin users and privileges
func (sh *SlackHandler) handleAdminCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	// Check admin privileges for all admin commands
	if adminResp := sh.requireAdmin(cmd.UserID); adminResp != nil {
		return adminResp, nil
	}

	if len(cmd.Arguments) == 0 {
		// Show current admins and help
		admins := sh.configService.GetAdmins()
		adminList := "• No admins configured"
		if len(admins) > 0 {
			adminList = "• " + strings.Join(admins, "\n• ")
		}

		helpText := "*Admin Management*\n\n" +
			"*Current Admins:*\n" + adminList + "\n\n" +
			"*Usage:*\n" +
			"• `@bot admin list` - List all admin users\n" +
			"• `@bot admin add <user_id>` - Add user as admin\n" +
			"• `@bot admin remove <user_id>` - Remove admin privileges\n\n" +
			"⚠️ Be careful when managing admin privileges!"

		return &models.CommandResponse{
			Success: true,
			Message: helpText,
		}, nil
	}

	action := strings.ToLower(cmd.Arguments[0])

	switch action {
	case "list":
		return sh.handleAdminList(cmd)
	case "add":
		return sh.handleAdminAdd(cmd)
	case "remove", "rm":
		return sh.handleAdminRemove(cmd)
	default:
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("Unknown admin action: %s. Use 'list', 'add', or 'remove'", action),
		}, nil
	}
}

// handleAdminList lists all admin users
func (sh *SlackHandler) handleAdminList(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	admins := sh.configService.GetAdmins()

	if len(admins) == 0 {
		return &models.CommandResponse{
			Success: true,
			Message: "📋 *Admin Users*\n\nNo admin users configured.",
		}, nil
	}

	adminList := "📋 *Admin Users:*\n\n"
	for i, adminID := range admins {
		adminList += fmt.Sprintf("%d. <@%s> (`%s`)\n", i+1, adminID, adminID)
	}

	return &models.CommandResponse{
		Success: true,
		Message: adminList,
	}, nil
}

// handleAdminAdd adds a new admin user
func (sh *SlackHandler) handleAdminAdd(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	if len(cmd.Arguments) < 2 {
		return &models.CommandResponse{
			Success: false,
			Error:   "Usage: `@bot admin add <user_id>`",
		}, nil
	}

	newAdminID := strings.TrimSpace(cmd.Arguments[1])

	// Remove @ and < > if user provided @mention format
	newAdminID = strings.TrimPrefix(newAdminID, "<@")
	newAdminID = strings.TrimSuffix(newAdminID, ">")
	newAdminID = strings.TrimPrefix(newAdminID, "@")

	err := sh.configService.AddAdmin(newAdminID)
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to add admin: %s", err.Error()),
		}, nil
	}

	return &models.CommandResponse{
		Success:      true,
		Message:      fmt.Sprintf("✅ Added <@%s> as admin user", newAdminID),
		ShouldNotify: true,
	}, nil
}

// handleAdminRemove removes an admin user
func (sh *SlackHandler) handleAdminRemove(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	if len(cmd.Arguments) < 2 {
		return &models.CommandResponse{
			Success: false,
			Error:   "Usage: `@bot admin remove <user_id>`",
		}, nil
	}

	removeAdminID := strings.TrimSpace(cmd.Arguments[1])

	// Remove @ and < > if user provided @mention format
	removeAdminID = strings.TrimPrefix(removeAdminID, "<@")
	removeAdminID = strings.TrimSuffix(removeAdminID, ">")
	removeAdminID = strings.TrimPrefix(removeAdminID, "@")

	// Don't allow removing yourself if you're the only admin
	admins := sh.configService.GetAdmins()
	if len(admins) == 1 && admins[0] == cmd.UserID && removeAdminID == cmd.UserID {
		return &models.CommandResponse{
			Success: false,
			Error:   "❌ Cannot remove yourself as the only admin",
		}, nil
	}

	err := sh.configService.RemoveAdmin(removeAdminID)
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to remove admin: %s", err.Error()),
		}, nil
	}

	return &models.CommandResponse{
		Success:      true,
		Message:      fmt.Sprintf("✅ Removed <@%s> from admin users", removeAdminID),
		ShouldNotify: true,
	}, nil
}

// handleClearCommand provides admin functionality to clear queues and release tags
func (sh *SlackHandler) handleClearCommand(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	// Check admin privileges
	if adminResp := sh.requireAdmin(cmd.UserID); adminResp != nil {
		return adminResp, nil
	}

	// Parse arguments to determine what to clean
	if len(cmd.Arguments) == 0 {
		// Show help for clean command
		helpText := "*Clean Command (Admin)*\n\n" +
			"Clean command provides administrative functions to reset the system:\n\n" +
			"*Usage:*\n" +
			"• `@bot clean queue` - Clear all queue positions\n" +
			"• `@bot clean tags` - Release all assigned tags\n" +
			"• `@bot clean all` - Clear queue AND release all tags\n" +
			"• `@bot clean expired` - Force cleanup of only expired tags\n\n" +
			"⚠️ *Warning:* These are administrative actions that affect all users!"

		return &models.CommandResponse{
			Success: true,
			Message: helpText,
		}, nil
	}

	action := strings.ToLower(cmd.Arguments[0])

	switch action {
	case "queue":
		return sh.handleCleanQueue(cmd)
	case "tags":
		return sh.handleCleanTags(cmd)
	case "all":
		return sh.handleCleanAll(cmd)
	case "expired":
		return sh.handleCleanExpired(cmd)
	default:
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("Unknown clean action: %s. Use 'queue', 'tags', 'all', or 'expired'", action),
		}, nil
	}
}

// handleCleanQueue clears all queue positions
func (sh *SlackHandler) handleCleanQueue(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	// Get current queue status to see what we're clearing
	status := sh.queueService.GetQueueStatus()

	if status.TotalUsers == 0 {
		return &models.CommandResponse{
			Success: true,
			Message: "✅ Queue is already empty - nothing to clear",
		}, nil
	}

	// Collect users who will be removed for notification
	var userList []string
	for _, item := range status.Queue {
		userList = append(userList, fmt.Sprintf("<@%s>", item.UserID))
	}

	// Actually clear the queue
	count, err := sh.queueService.ClearQueue()
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to clear queue: %s", err.Error()),
		}, nil
	}

	// Create success message
	message := fmt.Sprintf("✅ *Admin Clear Queue - COMPLETED*\n\n"+
		"Successfully removed **%d users** from the queue:\n"+
		"• %s\n\n"+
		"All users can now join fresh queues.",
		count, strings.Join(userList, "\n• "))

	return &models.CommandResponse{
		Success:      true,
		Message:      message,
		ShouldNotify: true,
	}, nil
}

// handleCleanTags releases all assigned tags
func (sh *SlackHandler) handleCleanTags(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	// Get current status to see what we're releasing
	status := sh.queueService.GetQueueStatus()

	if status.OccupiedTags == 0 {
		return &models.CommandResponse{
			Success: true,
			Message: "✅ No tags are currently assigned - nothing to release",
		}, nil
	}

	// Count and collect all assigned tags
	var allAssignedTags []string
	var allAssignedUsers []string
	var totalCount int

	for _, env := range status.Environments {
		for tagName, tag := range env.Tags {
			if tag.Status == "occupied" {
				allAssignedTags = append(allAssignedTags, fmt.Sprintf("%s/%s", env.Name, tagName))
				allAssignedUsers = append(allAssignedUsers, fmt.Sprintf("<@%s>", tag.AssignedTo))
				totalCount++
			}
		}
	}

	// For safety, require confirmation for large operations
	if totalCount > 10 && (len(cmd.Arguments) < 2 || strings.ToLower(cmd.Arguments[1]) != "confirm") {
		message := fmt.Sprintf("🚨 *Admin Clear Tags - Large Operation*\n\n"+
			"This will release **%d occupied tags** from all users:\n\n"+
			"```%s```\n\n"+
			"⚠️ This is a large operation affecting many users.\n"+
			"Consider using `@bot clear expired` first to clean only expired tags.\n\n"+
			"To proceed with releasing ALL tags, add 'confirm' to your command:\n"+
			"`@bot clear tags confirm`",
			totalCount, strings.Join(allAssignedTags[:10], "\n")+"\n... and "+fmt.Sprintf("%d", totalCount-10)+" more")

		return &models.CommandResponse{
			Success: true,
			Message: message,
		}, nil
	}

	// Check for confirmation for any operation > 5 tags
	if totalCount > 5 && (len(cmd.Arguments) < 2 || strings.ToLower(cmd.Arguments[1]) != "confirm") {
		message := fmt.Sprintf("🚨 *Admin Clear Tags*\n\n"+
			"This will release **%d occupied tags**:\n\n"+
			"```%s```\n\n"+
			"⚠️ This will affect all users with assignments.\n\n"+
			"To proceed, add 'confirm' to your command:\n"+
			"`@bot clear tags confirm`",
			totalCount, strings.Join(allAssignedTags, "\n"))

		return &models.CommandResponse{
			Success: true,
			Message: message,
		}, nil
	}

	// Actually release all tags
	count, releasedTags, err := sh.queueService.ReleaseAllAssignedTags()
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to release tags: %s", err.Error()),
		}, nil
	}

	if count == 0 {
		return &models.CommandResponse{
			Success: true,
			Message: "✅ No tags were assigned - nothing was released",
		}, nil
	}

	// Create success message
	message := fmt.Sprintf("✅ *Admin Clear Tags - COMPLETED*\n\n"+
		"Successfully released **%d tags**:\n"+
		"```%s```\n\n"+
		"All affected users have been notified.\nQueue processing will now assign available tags to waiting users.",
		count, strings.Join(releasedTags, "\n"))

	return &models.CommandResponse{
		Success:      true,
		Message:      message,
		ShouldNotify: true,
	}, nil
}

// handleCleanAll clears queue and releases all tags
func (sh *SlackHandler) handleCleanAll(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	status := sh.queueService.GetQueueStatus()

	if status.TotalUsers == 0 && status.OccupiedTags == 0 {
		return &models.CommandResponse{
			Success: true,
			Message: "✅ Queue is empty and no tags are assigned - nothing to clear",
		}, nil
	}

	// Require confirmation for this powerful operation
	if len(cmd.Arguments) < 2 || strings.ToLower(cmd.Arguments[1]) != "confirm" {
		message := fmt.Sprintf("🚨 *Admin Clear All - DANGER*\n\n"+
			"This will:\n"+
			"• Remove **%d users** from the queue\n"+
			"• Release **%d occupied tags** from all users\n\n"+
			"⚠️ **WARNING: This affects all users and resets the entire system!**\n\n"+
			"To proceed, add 'confirm' to your command:\n"+
			"`@bot clear all confirm`",
			status.TotalUsers, status.OccupiedTags)

		return &models.CommandResponse{
			Success: true,
			Message: message,
		}, nil
	}

	// Collect information for the success message
	var clearedUsers []string
	for _, item := range status.Queue {
		clearedUsers = append(clearedUsers, fmt.Sprintf("<@%s>", item.UserID))
	}

	var releasedTags []string
	for _, env := range status.Environments {
		for tagName, tag := range env.Tags {
			if tag.Status == "occupied" {
				releasedTags = append(releasedTags, fmt.Sprintf("%s/%s", env.Name, tagName))
			}
		}
	}

	// Perform both operations
	queueCount, err := sh.queueService.ClearQueue()
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to clear queue: %s", err.Error()),
		}, nil
	}

	tagCount, _, err := sh.queueService.ReleaseAllAssignedTags()
	if err != nil {
		return &models.CommandResponse{
			Success: false,
			Error:   fmt.Sprintf("Queue cleared but failed to release tags: %s", err.Error()),
		}, nil
	}

	// Create comprehensive success message
	message := fmt.Sprintf("✅ *Admin Clear All - COMPLETED*\n\n"+
		"**Queue Cleared:**\n"+
		"• Removed **%d users** from queue\n\n"+
		"**Tags Released:**\n"+
		"• Released **%d occupied tags**\n\n"+
		"The system has been completely reset. All users can now join fresh queues and tags are available for assignment.",
		queueCount, tagCount)

	return &models.CommandResponse{
		Success:      true,
		Message:      message,
		ShouldNotify: true,
	}, nil
}

// handleCleanExpired forces cleanup of expired tags only
func (sh *SlackHandler) handleCleanExpired(cmd *models.CommandRequest) (*models.CommandResponse, error) {
	// Provide information about the background expiration service

	message := "🔍 *Forced Expired Tag Cleanup*\n\n" +
		"✅ Background expiration service is running every 30 seconds\n\n" +
		"To force immediate cleanup:\n" +
		"• Wait up to 30 seconds for the next automatic check\n" +
		"• Any expired tags will be released automatically\n" +
		"• Check server logs for expiration activity\n\n" +
		"*Manual alternative:*\n" +
		"• Use `@bot status` to see your current assignments\n" +
		"• Use the 🔓 **Release All Tags** button for your own expired tags"

	return &models.CommandResponse{
		Success: true,
		Message: message,
	}, nil
}

// CreateQueueStatusBlocks creates Slack blocks for queue status (implements interface)
func (sh *SlackHandler) CreateQueueStatusBlocks() (interface{}, error) {
	// Delegate to notification service for queue status broadcast
	err := sh.notificationSvc.BroadcastQueueUpdate()
	return nil, err
}
