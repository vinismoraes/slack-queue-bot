package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"slack-queue-bot/internal/config"
	"slack-queue-bot/internal/database"
	"slack-queue-bot/internal/handlers"
	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
	"slack-queue-bot/internal/services"
	"slack-queue-bot/internal/utils"
)

// Application holds all dependencies for the application
type Application struct {
	SlackClient  *slack.Client
	SocketClient *socketmode.Client
	ChannelID    string
	DataDir      string
	ConfigPath   string
	DB           *database.DB
	ctx          context.Context
	cancel       context.CancelFunc

	// Services
	ConfigService               interfaces.ConfigService
	ValidationService           interfaces.ValidationService
	QueueService                interfaces.QueueService
	TagService                  interfaces.TagService
	NotificationService         interfaces.NotificationService
	BackgroundExpirationService *services.BackgroundExpirationService

	// Handlers
	SlackHandler interfaces.SlackHandler
	HTTPHandler  interfaces.HTTPHandler
}

// NewApplication creates a new application with all dependencies wired up
func NewApplication() (*Application, error) {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Could not load .env file: %v", err)
	}

	// Get environment variables
	token := os.Getenv("SLACK_BOT_TOKEN")
	appToken := os.Getenv("SLACK_APP_TOKEN")
	channelID := os.Getenv("SLACK_CHANNEL_ID")
	dataDir := os.Getenv("DATA_DIR")
	configPath := os.Getenv("CONFIG_PATH")

	// Set defaults
	if dataDir == "" {
		dataDir = "./data"
	}
	if configPath == "" {
		configPath = "./config.json"
	}

	// Validate required environment variables
	if token == "" || appToken == "" || channelID == "" {
		return nil, fmt.Errorf("missing required environment variables: SLACK_BOT_TOKEN, SLACK_APP_TOKEN, SLACK_CHANNEL_ID")
	}

	// Initialize Slack clients
	slackClient := slack.New(token, slack.OptionAppLevelToken(appToken))
	socketClient := socketmode.New(slackClient)

	// Initialize database
	db, err := database.NewDB(dataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database: %v", err)
	}

	// Initialize configuration manager with database
	configManager := config.NewManager(db)

	// Check if we need to migrate from JSON config
	if _, err := os.Stat(configPath); err == nil {
		log.Printf("Found JSON config file, attempting migration to database...")
		migrationManager := config.NewManagerFromJSON(configPath)
		migrationManager.SetDatabase(db)

		if err := migrationManager.MigrateFromJSON(configPath); err != nil {
			log.Printf("Warning: Failed to migrate JSON config: %v", err)
		} else {
			log.Printf("Successfully migrated configuration from JSON to database")
			// Optionally rename or backup the JSON file
			if err := os.Rename(configPath, configPath+".migrated"); err != nil {
				log.Printf("Warning: Could not rename migrated config file: %v", err)
			}
		}
	}

	// Load configuration from database
	if err := configManager.Load(); err != nil {
		return nil, fmt.Errorf("failed to load configuration: %v", err)
	}

	// Bootstrap admin user if none exist and ADMIN_USER_ID is set
	admins := configManager.GetAdmins()
	if len(admins) == 0 {
		bootstrapAdminID := os.Getenv("ADMIN_USER_ID")
		if bootstrapAdminID != "" {
			log.Printf("Bootstrapping first admin user: %s", bootstrapAdminID)
			if err := configManager.AddAdmin(bootstrapAdminID); err != nil {
				log.Printf("Warning: Failed to bootstrap admin user: %v", err)
			} else {
				log.Printf("Successfully added bootstrap admin user: %s", bootstrapAdminID)
			}
		} else {
			log.Printf("Warning: No admin users configured. Set ADMIN_USER_ID environment variable to bootstrap first admin.")
		}
	}

	config := configManager.Get()

	// Create repository wrapper for database operations
	repository := database.NewRepository(db)

	// Create services with dependency injection
	validationService := services.NewValidationService(&config.Settings)
	notificationService := services.NewNotificationService(slackClient, channelID, configManager, &config.Settings)
	queueService := services.NewQueueService(repository, validationService, notificationService, configManager)
	tagService := services.NewTagService(repository, validationService, notificationService, configManager)

	// Set queue service reference in notification service (to break circular dependency)
	notificationService.SetQueueService(queueService)

	// Create handlers
	slackHandler := handlers.NewSlackHandler(slackClient, queueService, tagService, notificationService, validationService, configManager, &config.Settings)
	httpHandler := handlers.NewHTTPHandler(queueService, configManager, tagService)

	// Create context for application lifecycle
	ctx, cancel := context.WithCancel(context.Background())

	return &Application{
		SlackClient:                 slackClient,
		SocketClient:                socketClient,
		ChannelID:                   channelID,
		DataDir:                     dataDir,
		ConfigPath:                  configPath,
		DB:                          db,
		ctx:                         ctx,
		cancel:                      cancel,
		ConfigService:               configManager,
		ValidationService:           validationService,
		QueueService:                queueService,
		TagService:                  tagService,
		NotificationService:         notificationService,
		BackgroundExpirationService: services.NewBackgroundExpirationService(repository, notificationService, queueService),
		SlackHandler:                slackHandler,
		HTTPHandler:                 httpHandler,
	}, nil
}

// Start starts the application
func (app *Application) Start() error {
	// Start the background expiration service
	log.Println("Starting background expiration service...")
	if err := app.BackgroundExpirationService.Start(); err != nil {
		log.Printf("Warning: Failed to start background expiration service: %v", err)
	}

	// Start socket mode client
	go func() {
		if err := app.SocketClient.Run(); err != nil {
			log.Printf("Socket mode client error: %v", err)
		}
	}()

	// Start socket mode listener
	go app.startSocketModeListener(app.ctx)

	// Start HTTP server
	return app.startHTTPServer()
}

// Stop gracefully shuts down the application
func (app *Application) Stop() error {
	// Stop expiration service
	if app.BackgroundExpirationService != nil {
		app.BackgroundExpirationService.Stop()
	}

	// Cancel context to stop all goroutines
	app.cancel()

	// Close database connection
	if err := app.DB.Close(); err != nil {
		log.Printf("Error closing database: %v", err)
		return err
	}

	return nil
}

// startSocketModeListener starts the socket mode event listener
func (app *Application) startSocketModeListener(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down socket mode listener")
			return
		case event := <-app.SocketClient.Events:
			switch event.Type {
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := event.Data.(slackevents.EventsAPIEvent)
				if !ok {
					log.Printf("Could not type cast event to EventsAPIEvent: %v", event)
					continue
				}
				app.SocketClient.Ack(*event.Request)

				if err := app.handleEventsAPI(eventsAPIEvent); err != nil {
					log.Printf("Error handling events API event: %v", err)
				}

			case socketmode.EventTypeInteractive:
				interactiveEvent, ok := event.Data.(slack.InteractionCallback)
				if !ok {
					log.Printf("Could not type cast event to InteractionCallback: %v", event)
					continue
				}
				app.SocketClient.Ack(*event.Request)

				if err := app.handleInteractive(interactiveEvent); err != nil {
					log.Printf("Error handling interactive event: %v", err)
				}
			}
		}
	}
}

// handleEventsAPI handles Events API events
func (app *Application) handleEventsAPI(event slackevents.EventsAPIEvent) error {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			slackEvent := &models.SlackEvent{
				Type:      "app_mention",
				UserID:    ev.User,
				ChannelID: ev.Channel,
				Text:      ev.Text,
				Timestamp: ev.TimeStamp,
			}
			response, err := app.SlackHandler.HandleAppMention(slackEvent)
			if err != nil {
				log.Printf("Error handling app mention: %v", err)
				return err
			}

			// Send response back to Slack
			if response != nil {
				if response.Success {
					if response.Message != "" {
						// Special handling for status command - send ephemeral message
						if response.Message == "status" && response.IsEphemeral {
							// Use CreateQueueStatusBlocksForUser to show user-specific buttons
							blocks, err := app.NotificationService.CreateQueueStatusBlocksForUser(ev.User)
							if err != nil {
								log.Printf("Error creating user-specific status blocks: %v", err)
								// Fallback to error message to user
								_, _, err := app.SlackClient.PostMessage(ev.User, slack.MsgOptionText("❌ Failed to get status", false))
								if err != nil {
									log.Printf("Error sending fallback error: %v", err)
								}
							} else {
								// Send user-specific status as ephemeral message
								_, err := app.SlackClient.PostEphemeral(ev.Channel, ev.User, slack.MsgOptionBlocks(blocks...))
								if err != nil {
									log.Printf("Error sending ephemeral status: %v", err)
								}
							}
						} else if response.IsEphemeral {
							// Send regular ephemeral text message
							_, err := app.SlackClient.PostEphemeral(ev.Channel, ev.User, slack.MsgOptionText(response.Message, false))
							if err != nil {
								log.Printf("Error sending ephemeral response: %v", err)
							}
						} else {
							// Send regular public message to the channel
							_, _, err := app.SlackClient.PostMessage(ev.Channel, slack.MsgOptionText(response.Message, false))
							if err != nil {
								log.Printf("Error sending response message: %v", err)
							}
						}
					}
				} else {
					// Send error message
					errorMsg := "❌ " + response.Error
					if response.IsEphemeral {
						_, err := app.SlackClient.PostEphemeral(ev.Channel, ev.User, slack.MsgOptionText(errorMsg, false))
						if err != nil {
							log.Printf("Error sending ephemeral error message: %v", err)
						}
					} else {
						_, _, err := app.SlackClient.PostMessage(ev.Channel, slack.MsgOptionText(errorMsg, false))
						if err != nil {
							log.Printf("Error sending error message: %v", err)
						}
					}
				}
			}
			return nil
		}
	}
	return nil
}

// handleInteractive handles interactive events (button clicks, etc.)
func (app *Application) handleInteractive(event slack.InteractionCallback) error {
	log.Printf("Handling interactive event: type=%s, user=%s", event.Type, event.User.ID)

	// Handle modal submissions
	if event.Type == slack.InteractionTypeViewSubmission {
		return app.handleModalSubmission(event)
	}

	// Extract the action ID from button clicks
	var actionID string
	if event.Type == slack.InteractionTypeBlockActions && len(event.ActionCallback.BlockActions) > 0 {
		actionID = event.ActionCallback.BlockActions[0].ActionID
		log.Printf("Button action ID: %s", actionID)
	}

	// Handle join_queue with modal
	if actionID == "join_queue" {
		return app.handleJoinQueueModal(event)
	}

	// Handle leave_queue - leave all queues immediately
	if actionID == "leave_queue" {
		// Check user's queue positions
		queueStatus := app.QueueService.GetQueueStatus()
		var userQueueItems []models.QueueItem
		for _, queueItem := range queueStatus.Queue {
			if queueItem.UserID == event.User.ID {
				userQueueItems = append(userQueueItems, queueItem)
			}
		}

		if len(userQueueItems) == 0 {
			// User not in queue
			_, _, err := app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText("❌ You are not in any queue", false))
			if err != nil {
				log.Printf("Error sending not in queue message: %v", err)
			}
			return nil
		}

		// Leave all queues with a single call (LeaveQueue removes ALL positions for a user)
		err := app.QueueService.LeaveQueue(event.User.ID)
		if err != nil {
			log.Printf("Error leaving queues: %v", err)
			errorMsg := fmt.Sprintf("❌ <@%s> failed to leave queues: %s", event.User.ID, err.Error())
			_, _, postErr := app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText(errorMsg, false))
			if postErr != nil {
				log.Printf("Error sending error message: %v", postErr)
			}
			return nil
		}

		// Prepare list of queues that were left for success message
		var leftQueues []string
		for _, queueItem := range userQueueItems {
			leftQueues = append(leftQueues, fmt.Sprintf("%s/%s", queueItem.Environment, queueItem.Tag))
		}

		// Send success message
		message := fmt.Sprintf("✅ <@%s> left %d queue(s): %s",
			event.User.ID, len(leftQueues), strings.Join(leftQueues, ", "))
		_, _, err = app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText(message, false))
		if err != nil {
			log.Printf("Error sending leave queue message: %v", err)
		}

		// Auto-update status after leaving queue (silent update only)
		err = app.NotificationService.UpdateExistingQueueStatus()
		if err != nil {
			log.Printf("Error silently updating queue status after leave: %v", err)
		}

		// Send updated ephemeral status to user (same as @bot status)
		blocks, statusErr := app.NotificationService.CreateQueueStatusBlocksForUser(event.User.ID)
		if statusErr != nil {
			log.Printf("Error creating updated status blocks after leave queue: %v", statusErr)
		} else {
			_, statusErr := app.SlackClient.PostEphemeral(app.ChannelID, event.User.ID, slack.MsgOptionBlocks(blocks...))
			if statusErr != nil {
				log.Printf("Error sending updated ephemeral status after leave queue: %v", statusErr)
			} else {
				log.Printf("Sent updated ephemeral status to user %s after leave queue (same as @bot status)", event.User.ID)
			}
		}

		return nil
	}

	// Handle manage_all - user has both assignments and queue positions
	if actionID == "manage_all" {
		return app.handleManageAllModal(event)
	}

	// Handle release_tags with modal for multiple tags
	if actionID == "release_tags" {
		return app.handleReleaseTagsModal(event)
	}

	// Handle confirm_leave_queue button click
	if actionID == "confirm_leave_queue" {
		// Leave the queue
		err := app.QueueService.LeaveQueue(event.User.ID)
		if err != nil {
			_, _, postErr := app.SlackClient.PostMessage(event.Channel.ID,
				slack.MsgOptionText(fmt.Sprintf("❌ Failed to leave queue: %s", err.Error()), false))
			if postErr != nil {
				log.Printf("Error sending error message: %v", postErr)
			}
			return err
		}

		// Send success message
		message := fmt.Sprintf("✅ <@%s> left the queue", event.User.ID)
		_, _, err = app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText(message, false))
		if err != nil {
			log.Printf("Error sending leave queue message: %v", err)
		}

		// Auto-update status after leaving queue (silent update only)
		err = app.NotificationService.UpdateExistingQueueStatus()
		if err != nil {
			log.Printf("Error silently updating queue status after leave: %v", err)
		}

		// Close the modal
		return app.closeModal(event)
	}

	// Handle cancel_leave_queue button click
	if actionID == "cancel_leave_queue" {
		// Just close the modal
		return app.closeModal(event)
	}

	// Handle individual tag release buttons
	if strings.HasPrefix(actionID, "release_tag_") {
		return app.handleReleaseIndividualTag(event, actionID)
	}

	// Handle release_all_tags button click
	if actionID == "release_all_tags" {
		releasedTags, err := app.QueueService.ReleaseUserTagsBulk(event.User.ID)
		if err != nil {
			_, _, postErr := app.SlackClient.PostMessage(event.Channel.ID,
				slack.MsgOptionText(fmt.Sprintf("❌ Failed to release tags: %s", err.Error()), false))
			if postErr != nil {
				log.Printf("Error sending error message: %v", postErr)
			}
			return err
		}

		// Send consolidated message
		message := fmt.Sprintf("✅ <@%s> released all assignments: %s",
			event.User.ID, strings.Join(releasedTags, ", "))
		_, _, err = app.SlackClient.PostMessage(app.ChannelID, slack.MsgOptionText(message, false))
		if err != nil {
			log.Printf("Error sending release all message: %v", err)
		}

		// Auto-update status after tag releases (silent update only)
		err = app.NotificationService.UpdateExistingQueueStatus()
		if err != nil {
			log.Printf("Error silently updating queue status after release all: %v", err)
		}

		// Send updated ephemeral status to user (same as @bot status)
		blocks, statusErr := app.NotificationService.CreateQueueStatusBlocksForUser(event.User.ID)
		if statusErr != nil {
			log.Printf("Error creating updated status blocks after release all: %v", statusErr)
		} else {
			_, statusErr := app.SlackClient.PostEphemeral(app.ChannelID, event.User.ID, slack.MsgOptionBlocks(blocks...))
			if statusErr != nil {
				log.Printf("Error sending updated ephemeral status after release all: %v", statusErr)
			} else {
				log.Printf("Sent updated ephemeral status to user %s after release all (same as @bot status)", event.User.ID)
			}
		}

		// Update the modal to show completion instead of closing
		return app.showReleaseAllCompletionModal(event, releasedTags)
	}

	// Handle leave_all_queues button click
	if actionID == "leave_all_queues" {
		// Leave all queues for the user
		queueStatus := app.QueueService.GetQueueStatus()
		var userQueueItems []models.QueueItem
		for _, queueItem := range queueStatus.Queue {
			if queueItem.UserID == event.User.ID {
				userQueueItems = append(userQueueItems, queueItem)
			}
		}

		if len(userQueueItems) == 0 {
			// User not in queue
			_, _, err := app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText("❌ You are not in any queue", false))
			if err != nil {
				log.Printf("Error sending not in queue message: %v", err)
			}
			return app.closeModal(event)
		}

		// Leave all queues with a single call (LeaveQueue removes ALL positions for a user)
		err := app.QueueService.LeaveQueue(event.User.ID)
		if err != nil {
			log.Printf("Error leaving queues: %v", err)
			errorMsg := fmt.Sprintf("❌ <@%s> failed to leave queues: %s", event.User.ID, err.Error())
			_, _, postErr := app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText(errorMsg, false))
			if postErr != nil {
				log.Printf("Error sending error message: %v", postErr)
			}
			return app.closeModal(event)
		}

		// Prepare list of queues that were left for success message
		var leftQueues []string
		for _, queueItem := range userQueueItems {
			leftQueues = append(leftQueues, fmt.Sprintf("%s/%s", queueItem.Environment, queueItem.Tag))
		}

		// Send success message
		message := fmt.Sprintf("✅ <@%s> left %d queue(s): %s",
			event.User.ID, len(leftQueues), strings.Join(leftQueues, ", "))
		_, _, err = app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText(message, false))
		if err != nil {
			log.Printf("Error sending leave queue message: %v", err)
		}

		// Auto-update status after leaving queue (silent update only)
		err = app.NotificationService.UpdateExistingQueueStatus()
		if err != nil {
			log.Printf("Error silently updating queue status after leave: %v", err)
		}

		// Close the modal since all queues are left
		return app.closeModal(event)
	}

	// Handle abandon_all button click (both release tags and leave queues)
	if actionID == "abandon_all" {
		// Release all tags
		releasedTags, err := app.QueueService.ReleaseUserTagsBulk(event.User.ID)
		if err != nil {
			log.Printf("Error releasing tags: %v", err)
		}

		// Leave all queues
		queueStatus := app.QueueService.GetQueueStatus()
		var userQueueItems []models.QueueItem
		for _, queueItem := range queueStatus.Queue {
			if queueItem.UserID == event.User.ID {
				userQueueItems = append(userQueueItems, queueItem)
			}
		}

		var leftQueues []string
		if len(userQueueItems) > 0 {
			// Leave all queues with a single call
			err := app.QueueService.LeaveQueue(event.User.ID)
			if err != nil {
				log.Printf("Error leaving queues during abandon all: %v", err)
			} else {
				// Prepare list of queues that were left for success message
				for _, queueItem := range userQueueItems {
					leftQueues = append(leftQueues, fmt.Sprintf("%s/%s", queueItem.Environment, queueItem.Tag))
				}
			}
		}

		// Send consolidated message
		var messageParts []string
		if len(releasedTags) > 0 {
			messageParts = append(messageParts, fmt.Sprintf("🔓 Released %d tag(s): %s", len(releasedTags), strings.Join(releasedTags, ", ")))
		}
		if len(leftQueues) > 0 {
			messageParts = append(messageParts, fmt.Sprintf("🚪 Left %d queue(s): %s", len(leftQueues), strings.Join(leftQueues, ", ")))
		}

		if len(messageParts) > 0 {
			message := fmt.Sprintf("💥 <@%s> abandoned all: %s",
				event.User.ID, strings.Join(messageParts, " | "))
			_, _, err := app.SlackClient.PostMessage(app.ChannelID, slack.MsgOptionText(message, false))
			if err != nil {
				log.Printf("Error sending abandon all message: %v", err)
			}

			// Auto-update status (silent update only)
			err = app.NotificationService.UpdateExistingQueueStatus()
			if err != nil {
				log.Printf("Error silently updating queue status after abandon all: %v", err)
			}

			// Send updated ephemeral status to user (same as @bot status)
			blocks, statusErr := app.NotificationService.CreateQueueStatusBlocksForUser(event.User.ID)
			if statusErr != nil {
				log.Printf("Error creating updated status blocks after abandon all: %v", statusErr)
			} else {
				_, statusErr := app.SlackClient.PostEphemeral(app.ChannelID, event.User.ID, slack.MsgOptionBlocks(blocks...))
				if statusErr != nil {
					log.Printf("Error sending updated ephemeral status after abandon all: %v", statusErr)
				} else {
					log.Printf("Sent updated ephemeral status to user %s after abandon all (same as @bot status)", event.User.ID)
				}
			}
		}

		// Close the modal since everything is abandoned
		return app.closeModal(event)
	}

	// Handle refresh environment list button
	if actionID == "refresh_environment_list" {
		log.Printf("Refresh environment list clicked for user %s", event.User.ID)
		return app.refreshJoinQueueModal(event)
	}

	// Handle environment selection from step 1 modal
	if strings.HasPrefix(actionID, "select_env_") {
		log.Printf("Environment button clicked - TriggerID: %s, UserID: %s, ChannelID: %s", event.TriggerID, event.User.ID, event.Channel.ID)
		log.Printf("Event details: %+v", event)

		// Extract environment name from action ID (remove "select_env_" prefix and any trailing index)
		envName := strings.TrimPrefix(actionID, "select_env_")
		// Remove the trailing index (_X) if present
		if lastUnderscore := strings.LastIndex(envName, "_"); lastUnderscore != -1 {
			// Check if everything after the last underscore is a number
			if _, err := strconv.Atoi(envName[lastUnderscore+1:]); err == nil {
				envName = envName[:lastUnderscore]
			}
		}
		log.Printf("Extracted environment name: %s", envName)
		return app.handleJoinQueueStep2Modal(event, envName)
	}

	slackEvent := &models.SlackEvent{
		Type:      actionID, // Use the button action ID as the event type
		UserID:    event.User.ID,
		ChannelID: event.Channel.ID,
		Text:      "",
		Timestamp: "",
	}

	response, err := app.SlackHandler.HandleInteraction(slackEvent)
	if err != nil {
		log.Printf("Error handling interaction: %v", err)
		return err
	}

	// Send response back to Slack if there is one
	if response != nil {
		if response.Success {
			if response.Message != "" {
				_, _, err := app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText(response.Message, false))
				if err != nil {
					log.Printf("Error sending interaction response: %v", err)
				}
			}

			// Auto-update status after actions that change queue state
			if actionID == "leave_queue" || actionID == "check_position" {
				// Use silent update for user actions
				err := app.NotificationService.UpdateExistingQueueStatus()
				if err != nil {
					log.Printf("Error silently updating queue status after %s: %v", actionID, err)
				}
			} else if actionID == "assign_next" {
				// Use regular broadcast for admin actions
				err := app.NotificationService.BroadcastOrUpdateQueueStatus()
				if err != nil {
					log.Printf("Error auto-updating queue status after %s: %v", actionID, err)
				}
			}
		} else {
			// Send error message
			errorMsg := "❌ " + response.Error
			_, _, err := app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText(errorMsg, false))
			if err != nil {
				log.Printf("Error sending interaction error: %v", err)
			}
		}
	}

	return nil
}

// handleModalSubmission handles modal form submissions
func (app *Application) handleModalSubmission(event slack.InteractionCallback) error {
	log.Printf("Handling modal submission: callbackID='%s', user=%s", event.CallbackID, event.User.ID)
	log.Printf("View details: ID=%s, CallbackID=%s", event.View.ID, event.View.CallbackID)

	// Check both event and view callback IDs
	callbackID := event.CallbackID
	if callbackID == "" {
		callbackID = event.View.CallbackID
		log.Printf("Using view callback ID: %s", callbackID)
	}

	if callbackID == "join_queue_modal" || strings.HasPrefix(callbackID, "join_queue_step2_") || strings.HasPrefix(callbackID, "join_step2_") {
		return app.handleJoinQueueModalSubmission(event)
	}

	if callbackID == "release_tags_modal" {
		return app.handleReleaseTagsModalSubmission(event)
	}

	log.Printf("No handler found for callback ID: %s", callbackID)
	return nil
}

// handleJoinQueueModalSubmission processes the join queue modal form data
func (app *Application) handleJoinQueueModalSubmission(event slack.InteractionCallback) error {
	log.Printf("Processing join queue modal submission for user %s", event.User.ID)

	// Extract form values
	values := event.View.State.Values

	// Get callback ID (use view callback ID if event callback ID is empty)
	callbackID := event.CallbackID
	if callbackID == "" {
		callbackID = event.View.CallbackID
	}
	log.Printf("Using callback ID: %s", callbackID)

	// Get environment
	var environment string
	// Try to extract from new callback ID format (join_step2_*)
	if strings.HasPrefix(callbackID, "join_step2_") {
		envName := strings.TrimPrefix(callbackID, "join_step2_")
		// Convert underscores back to dashes
		environment = strings.ReplaceAll(envName, "_", "-")
	}
	// Try to extract from old callback ID format (join_queue_step2_*)
	if environment == "" && strings.HasPrefix(callbackID, "join_queue_step2_") {
		environment = strings.TrimPrefix(callbackID, "join_queue_step2_")
	}
	// Try step 2 hidden field (backward compatibility)
	if environment == "" {
		if envBlock, exists := values["environment_hidden"]; exists {
			if envValue, exists := envBlock["environment"]; exists {
				environment = strings.TrimSpace(envValue.Value)
			}
		}
	}
	// Fallback to old dropdown (for backward compatibility)
	if environment == "" {
		if envBlock, exists := values["environment_select"]; exists {
			if envValue, exists := envBlock["environment"]; exists {
				environment = envValue.SelectedOption.Value
			}
		}
	}

	// Get tags - handle multi-select (required)
	var tags []string
	if tagBlock, exists := values["tag_select_block"]; exists {
		if tagValue, exists := tagBlock["tag_select_action"]; exists {
			if tagValue.SelectedOptions != nil {
				for _, option := range tagValue.SelectedOptions {
					tags = append(tags, strings.TrimSpace(option.Value))
				}
			}
		}
	}

	// Fallback to old single tag input methods for backward compatibility
	if len(tags) == 0 {
		if tagBlock, exists := values["tag_input_block"]; exists {
			if tagValue, exists := tagBlock["tag_action"]; exists {
				tag := strings.TrimSpace(tagValue.Value)
				if tag != "" {
					tags = append(tags, tag)
				}
			}
		}
		if len(tags) == 0 {
			if tagBlock, exists := values["tag_input"]; exists {
				if tagValue, exists := tagBlock["tag"]; exists {
					tag := strings.TrimSpace(tagValue.Value)
					if tag != "" {
						tags = append(tags, tag)
					}
				}
			}
		}
	}

	// Get duration
	// Get config-driven default duration (smallest configured)
	configSettings := app.ConfigService.GetSettings()
	defaultDurationStr := configSettings.MinDuration.String()
	var durationStr string = defaultDurationStr
	if durationBlock, exists := values["duration_input_block"]; exists {
		if durationValue, exists := durationBlock["duration_action"]; exists {
			durationStr = durationValue.SelectedOption.Value
		}
	}
	// Fallback to old action IDs for backward compatibility
	if durationStr == "3h" { // only fallback if still default
		if durationBlock, exists := values["duration_select_block"]; exists {
			if durationValue, exists := durationBlock["duration_select_action"]; exists {
				durationStr = durationValue.SelectedOption.Value
			}
		}
		if durationStr == "3h" {
			if durationBlock, exists := values["duration_select"]; exists {
				if durationValue, exists := durationBlock["duration"]; exists {
					durationStr = durationValue.SelectedOption.Value
				}
			}
		}
	}

	// Parse duration
	duration, err := time.ParseDuration(durationStr)
	if err != nil {
		log.Printf("Error parsing duration %s: %v", durationStr, err)
		duration = 3 * time.Hour // fallback
	}

	// Get user info
	user, err := app.SlackClient.GetUserInfo(event.User.ID)
	if err != nil {
		log.Printf("Error getting user info: %v", err)
		user = &slack.User{Name: event.User.ID}
	}

	// Process each selected tag
	var successfulTags []string
	// Suppress individual notifications if processing multiple tags
	if len(tags) > 1 {
		app.QueueService.SetSuppressIndividualNotifications(true)
		defer app.QueueService.SetSuppressIndividualNotifications(false)
	}

	// Process all tags first to determine immediate assignments vs queue joins
	var immediateAssignments []string
	var queueJoins []string
	var errorMessages []string

	// First pass: Try to assign immediately for all tags
	for _, tag := range tags {
		log.Printf("Processing tag: %s", tag)

		// Check if tag is available for immediate assignment
		queueStatus := app.QueueService.GetQueueStatus()
		var tagAvailable bool
		for _, env := range queueStatus.Environments {
			if env.Name == environment {
				if tagObj, exists := env.Tags[tag]; exists && tagObj.Status == "available" {
					tagAvailable = true
					break
				}
			}
		}

		if tagAvailable {
			// Tag is available - try immediate assignment
			err = app.QueueService.JoinQueue(event.User.ID, user.Name, environment, tag, duration)
			if err != nil {
				log.Printf("Error in immediate assignment for tag %s: %v", tag, err)
				errorMessages = append(errorMessages, fmt.Sprintf("%s (already assigned/queued)", tag))
				continue
			}

			// Check if assignment was successful
			userPosition := app.QueueService.GetUserPosition(event.User.ID)
			if userPosition.Position == -1 {
				// User was immediately assigned
				log.Printf("User %s was immediately assigned to %s/%s", event.User.ID, environment, tag)
				immediateAssignments = append(immediateAssignments, tag)
			} else {
				// User was added to queue (shouldn't happen if tag was available)
				log.Printf("User %s was added to queue for %s/%s (unexpected)", event.User.ID, environment, tag)
				queueJoins = append(queueJoins, tag)
			}
		} else {
			// Tag is not available - add to queue
			err = app.QueueService.JoinQueue(event.User.ID, user.Name, environment, tag, duration)
			if err != nil {
				log.Printf("Error joining queue for tag %s: %v", tag, err)
				errorMessages = append(errorMessages, fmt.Sprintf("%s (queue full/already queued)", tag))
				continue
			}
			log.Printf("User %s added to queue for %s/%s", event.User.ID, environment, tag)
			queueJoins = append(queueJoins, tag)
		}

		successfulTags = append(successfulTags, tag)
	}

	// Send consolidated summary message for multi-tag scenarios
	if len(tags) > 1 {
		var summaryParts []string

		// Add immediate assignments
		if len(immediateAssignments) > 0 {
			summaryParts = append(summaryParts, fmt.Sprintf("🎉 *Immediately assigned:* %s", strings.Join(immediateAssignments, ", ")))
		}

		// Add queue joins
		if len(queueJoins) > 0 {
			summaryParts = append(summaryParts, fmt.Sprintf("⏳ *Added to queue:* %s", strings.Join(queueJoins, ", ")))
		}

		// Add errors
		if len(errorMessages) > 0 {
			summaryParts = append(summaryParts, fmt.Sprintf("❌ *Failed:* %s", strings.Join(errorMessages, ", ")))
		}

		// Create consolidated message
		displayEnvName := strings.ReplaceAll(environment, "-", " ")
		displayEnvName = strings.Title(displayEnvName)
		summaryText := fmt.Sprintf("*<@%s> requested tags in %s (%s):*\n%s",
			event.User.ID, displayEnvName, utils.FormatDuration(duration), strings.Join(summaryParts, "\n"))

		_, _, err = app.SlackClient.PostMessage(app.ChannelID, slack.MsgOptionText(summaryText, false))
		if err != nil {
			log.Printf("Error sending consolidated summary message: %v", err)
		}
	} else {
		// Single tag - use existing individual notification logic
		// Send immediate assignment message if any (only for multi-tag scenarios)
		if len(immediateAssignments) > 0 && len(tags) > 1 {
			message := fmt.Sprintf("🎉 <@%s> immediately assigned: *%s* in *%s* (%s)",
				event.User.ID, strings.Join(immediateAssignments, ", "), environment, utils.FormatDuration(duration))
			_, _, err = app.SlackClient.PostMessage(app.ChannelID, slack.MsgOptionText(message, false))
			if err != nil {
				log.Printf("Error sending immediate assignment message: %v", err)
			}
		}

		// Send queue join message if any
		if len(queueJoins) > 0 {
			message := fmt.Sprintf("✅ <@%s> joined the queue for *%s / %s* (%s)",
				event.User.ID, environment, strings.Join(queueJoins, ", "), utils.FormatDuration(duration))
			_, _, err = app.SlackClient.PostMessage(app.ChannelID, slack.MsgOptionText(message, false))
			if err != nil {
				log.Printf("Error sending queue join message: %v", err)
			}
		}
	}

	// If no tags were processed successfully, return an error
	if len(successfulTags) == 0 && len(queueJoins) == 0 {
		return fmt.Errorf("failed to process any tags")
	}

	// Send updated ephemeral status to user after successful join operations (same as @bot status)
	if len(successfulTags) > 0 || len(queueJoins) > 0 {
		// Small delay to ensure all database updates are committed
		time.Sleep(100 * time.Millisecond)

		// Send the exact same ephemeral status message as "@bot status" with updated info
		blocks, err := app.NotificationService.CreateQueueStatusBlocksForUser(event.User.ID)
		if err != nil {
			log.Printf("Error creating updated status blocks after join: %v", err)
		} else {
			// Use the same channel as the modal interaction (should be the main channel)
			channelID := app.ChannelID // Use the main channel ID from config
			_, err := app.SlackClient.PostEphemeral(channelID, event.User.ID, slack.MsgOptionBlocks(blocks...))
			if err != nil {
				log.Printf("Error sending updated ephemeral status after join: %v", err)
			} else {
				log.Printf("Sent updated ephemeral status to user %s after join (same as @bot status)", event.User.ID)
			}
		}
	}

	return nil
}

// handleJoinQueueModal opens the first step modal for environment selection
func (app *Application) handleJoinQueueModal(event slack.InteractionCallback) error {
	log.Printf("Opening join queue modal (step 1) for user %s", event.User.ID)

	// Auto-refresh: Get fresh environment data before opening
	return app.openJoinQueueModalWithFreshData(event.TriggerID, event.User.ID)
}

// openJoinQueueModalWithFreshData opens the modal with fresh environment data
func (app *Application) openJoinQueueModalWithFreshData(triggerID, userID string) error {
	// Get environments from queue service
	queueStatus := app.QueueService.GetQueueStatus()

	// Validate that we have environments
	if queueStatus == nil || queueStatus.Environments == nil || len(queueStatus.Environments) == 0 {
		log.Printf("Error: No environments available for modal")
		return fmt.Errorf("no environments available")
	}

	// Sort environments by name for consistent ordering
	var sortedEnvNames []string
	for envName := range queueStatus.Environments {
		if envName != "" { // Skip empty environment names
			sortedEnvNames = append(sortedEnvNames, envName)
		}
	}
	sort.Strings(sortedEnvNames)

	// Validate that we have valid environments
	if len(sortedEnvNames) == 0 {
		log.Printf("Error: No valid environments found")
		return fmt.Errorf("no valid environments found")
	}

	var environmentButtons []slack.BlockElement
	for _, envName := range sortedEnvNames {
		env := queueStatus.Environments[envName]
		// Count available tags in this environment
		availableCount := 0
		userHasAssignment := false
		userInQueue := false
		var userQueuePosition int

		for _, tag := range env.Tags {
			if tag.Status == "available" {
				availableCount++
			}
			if tag.AssignedTo == userID && tag.Status == "occupied" {
				userHasAssignment = true
			}
		}

		// Check if user is in queue for this environment
		for i, queueItem := range queueStatus.Queue {
			if queueItem.UserID == userID && queueItem.Environment == env.Name {
				userInQueue = true
				userQueuePosition = i + 1
				break
			}
		}

		// Use display name if available, otherwise use formatted environment name
		var displayName string
		if env.DisplayName != "" {
			displayName = env.DisplayName
		} else {
			displayName = strings.ReplaceAll(env.Name, "-", " ")
			displayName = strings.Title(displayName)
		}

		var buttonText string
		var buttonStyle string

		if userHasAssignment {
			buttonText = fmt.Sprintf("✅ %s", displayName)
			buttonStyle = "default" // Default for existing assignment (informational)
		} else if userInQueue {
			buttonText = fmt.Sprintf("⏳ %s (#%d)", displayName, userQueuePosition)
			buttonStyle = "default" // Default for existing queue position (informational)
		} else if availableCount > 0 {
			buttonText = fmt.Sprintf("%s (%d)", displayName, availableCount)
			buttonStyle = "primary" // Green for available
		} else {
			buttonText = fmt.Sprintf("🚫 %s", displayName)
			buttonStyle = "danger" // Red for no availability (actual blocking condition)
		}

		// Ensure button text isn't too long (Slack limit is ~75 characters for button text)
		if len(buttonText) > 70 {
			if userHasAssignment {
				buttonText = "✅ " + displayName[:65] + "..."
			} else if userInQueue {
				buttonText = fmt.Sprintf("⏳ %s... (#%d)", displayName[:60], userQueuePosition)
			} else if availableCount > 0 {
				buttonText = fmt.Sprintf("%s... (%d)", displayName[:60], availableCount)
			} else {
				buttonText = "🚫 " + displayName[:65] + "..."
			}
		}

		button := slack.NewButtonBlockElement(fmt.Sprintf("select_env_%s", env.Name), env.Name,
			slack.NewTextBlockObject("plain_text", buttonText, true, false))
		button.Style = slack.Style(buttonStyle)

		environmentButtons = append(environmentButtons, button)
	}

	// Create blocks for the modal
	var blocks []slack.Block

	// Header with refresh option
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", "*🎯 Select Environment*\nChoose the environment where you want to join the queue:", false, false),
		nil,
		&slack.Accessory{
			ButtonElement: &slack.ButtonBlockElement{
				Type:     slack.METButton,
				ActionID: "refresh_environment_list",
				Text:     slack.NewTextBlockObject("plain_text", "🔄 Refresh", false, false),
				Style:    slack.StyleDefault,
				Value:    "refresh",
			},
		},
	))

	// Add buttons in rows of 2 for better grid layout
	for i := 0; i < len(environmentButtons); i += 2 {
		var rowButtons []slack.BlockElement
		rowButtons = append(rowButtons, environmentButtons[i])

		// Add second button if available
		if i+1 < len(environmentButtons) {
			rowButtons = append(rowButtons, environmentButtons[i+1])
		}

		actionBlock := slack.NewActionBlock("", rowButtons...)
		blocks = append(blocks, actionBlock)
	}

	modalRequest := slack.ModalViewRequest{
		Type:       slack.ViewType("modal"),
		Title:      slack.NewTextBlockObject("plain_text", "Join Queue - Step 1", false, false),
		Close:      slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		Blocks:     slack.Blocks{BlockSet: blocks},
		CallbackID: "join_queue_step1_modal",
	}

	// Open the modal
	_, err := app.SlackClient.OpenView(triggerID, modalRequest)
	if err != nil {
		log.Printf("Error opening modal: %v", err)
		return err
	}

	log.Printf("Successfully opened join queue modal (step 1) for user %s", userID)
	return nil
}

// refreshJoinQueueModal refreshes the environment list in the step 1 modal
func (app *Application) refreshJoinQueueModal(event slack.InteractionCallback) error {
	log.Printf("Refreshing join queue modal for user %s", event.User.ID)

	// Get fresh environment data
	queueStatus := app.QueueService.GetQueueStatus()

	// Sort environments by name for consistent ordering
	var sortedEnvNames []string
	for envName := range queueStatus.Environments {
		sortedEnvNames = append(sortedEnvNames, envName)
	}
	sort.Strings(sortedEnvNames)

	var environmentButtons []slack.BlockElement
	for _, envName := range sortedEnvNames {
		env := queueStatus.Environments[envName]
		// Count available tags in this environment
		availableCount := 0
		userHasAssignment := false
		userInQueue := false
		var userQueuePosition int

		for _, tag := range env.Tags {
			if tag.Status == "available" {
				availableCount++
			}
			if tag.AssignedTo == event.User.ID && tag.Status == "occupied" {
				userHasAssignment = true
			}
		}

		// Check if user is in queue for this environment
		for i, queueItem := range queueStatus.Queue {
			if queueItem.UserID == event.User.ID && queueItem.Environment == env.Name {
				userInQueue = true
				userQueuePosition = i + 1
				break
			}
		}

		// Use display name if available, otherwise use formatted environment name
		var displayName string
		if env.DisplayName != "" {
			displayName = env.DisplayName
		} else {
			displayName = strings.ReplaceAll(env.Name, "-", " ")
			displayName = strings.Title(displayName)
		}

		var buttonText string
		var buttonStyle string

		if userHasAssignment {
			buttonText = fmt.Sprintf("✅ %s", displayName)
			buttonStyle = "default" // Default for existing assignment (informational)
		} else if userInQueue {
			buttonText = fmt.Sprintf("⏳ %s (#%d)", displayName, userQueuePosition)
			buttonStyle = "default" // Default for existing queue position (informational)
		} else if availableCount > 0 {
			buttonText = fmt.Sprintf("%s (%d)", displayName, availableCount)
			buttonStyle = "primary" // Green for available
		} else {
			buttonText = fmt.Sprintf("🚫 %s", displayName)
			buttonStyle = "danger" // Red for no availability (actual blocking condition)
		}

		// Ensure button text isn't too long (Slack limit is ~75 characters for button text)
		if len(buttonText) > 70 {
			if userHasAssignment {
				buttonText = "✅ " + displayName[:65] + "..."
			} else if userInQueue {
				buttonText = fmt.Sprintf("⏳ %s... (#%d)", displayName[:60], userQueuePosition)
			} else if availableCount > 0 {
				buttonText = fmt.Sprintf("%s... (%d)", displayName[:60], availableCount)
			} else {
				buttonText = "🚫 " + displayName[:65] + "..."
			}
		}

		button := slack.NewButtonBlockElement(fmt.Sprintf("select_env_%s", env.Name), env.Name,
			slack.NewTextBlockObject("plain_text", buttonText, true, false))
		button.Style = slack.Style(buttonStyle)

		environmentButtons = append(environmentButtons, button)
	}

	// Create blocks for the refreshed modal
	var blocks []slack.Block

	// Header with refresh option
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", "*🎯 Select Environment*\nChoose the environment where you want to join the queue:", false, false),
		nil,
		&slack.Accessory{
			ButtonElement: &slack.ButtonBlockElement{
				Type:     slack.METButton,
				ActionID: "refresh_environment_list",
				Text:     slack.NewTextBlockObject("plain_text", "🔄 Refresh", false, false),
				Style:    slack.StyleDefault,
				Value:    "refresh",
			},
		},
	))

	// Add buttons in rows of 2 for better grid layout
	for i := 0; i < len(environmentButtons); i += 2 {
		var rowButtons []slack.BlockElement
		rowButtons = append(rowButtons, environmentButtons[i])

		// Add second button if available
		if i+1 < len(environmentButtons) {
			rowButtons = append(rowButtons, environmentButtons[i+1])
		}

		actionBlock := slack.NewActionBlock("", rowButtons...)
		blocks = append(blocks, actionBlock)
	}

	// Update the existing modal with fresh data
	modalRequest := slack.ModalViewRequest{
		Type:       slack.ViewType("modal"),
		Title:      slack.NewTextBlockObject("plain_text", "Join Queue - Step 1", false, false),
		Close:      slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		Blocks:     slack.Blocks{BlockSet: blocks},
		CallbackID: "join_queue_step1_modal",
	}

	// Update the modal view
	_, err := app.SlackClient.UpdateView(modalRequest, "", "", event.View.ID)
	if err != nil {
		log.Printf("Error updating modal: %v", err)
		return err
	}

	log.Printf("Successfully refreshed join queue modal for user %s", event.User.ID)
	return nil
}

// handleJoinQueueStep2Modal opens the second step modal for tag and duration selection
func (app *Application) handleJoinQueueStep2Modal(event slack.InteractionCallback, environment string) error {
	log.Printf("Opening join queue modal (step 2) for user %s, environment %s", event.User.ID, environment)

	// Check user's current status for this specific environment
	queueStatus := app.QueueService.GetQueueStatus()

	// Check if user is already in queue for this environment (this should still block)
	for _, queueItem := range queueStatus.Queue {
		if queueItem.UserID == event.User.ID && queueItem.Environment == environment {
			errorModal := slack.ModalViewRequest{
				Type:  slack.ViewType("modal"),
				Title: slack.NewTextBlockObject("plain_text", "Already in Queue", false, false),
				Close: slack.NewTextBlockObject("plain_text", "Close", false, false),
				Blocks: slack.Blocks{BlockSet: []slack.Block{
					slack.NewSectionBlock(
						slack.NewTextBlockObject("mrkdwn",
							fmt.Sprintf("⏳ *Already in queue*\n\nYou are already queued for *%s/%s* in this environment",
								queueItem.Environment, queueItem.Tag), false, false),
						nil, nil,
					),
				}},
			}
			_, err := app.SlackClient.UpdateView(errorModal, "", "", event.View.ID)
			return err
		}
	}

	// Get tags for the selected environment
	status := app.QueueService.GetQueueStatus()
	var tagOptions []*slack.OptionBlockObject
	var occupiedTags []string
	var availableCount int

	if env, exists := status.Environments[environment]; exists {
		// Collect all tags and sort by name
		var allTags []string
		for tagName := range env.Tags {
			allTags = append(allTags, tagName)
		}
		sort.Strings(allTags)

		// Create options and count statuses
		for _, tagName := range allTags {
			tag := env.Tags[tagName]
			// Create more readable tag display name
			displayTagName := strings.ReplaceAll(tagName, "-", " ")
			displayTagName = strings.Title(displayTagName)

			if tag.Status == "available" {
				availableCount++
				// Add available tags to multi-select options
				tagOptions = append(tagOptions, slack.NewOptionBlockObject(
					tagName,
					slack.NewTextBlockObject("plain_text", displayTagName, false, false),
					slack.NewTextBlockObject("plain_text", "🟢 available", false, false),
				))
			} else if tag.Status == "occupied" {
				// Don't allow user to select tags they already have assigned
				if tag.AssignedTo == event.User.ID {
					continue // Skip tags already assigned to this user
				}

				// Add occupied tags (assigned to others) - users can queue for them
				occupiedTags = append(occupiedTags, displayTagName)
				tagOptions = append(tagOptions, slack.NewOptionBlockObject(
					tagName,
					slack.NewTextBlockObject("plain_text", displayTagName, false, false),
					slack.NewTextBlockObject("plain_text", "🔵 in use (queue)", false, false),
				))
			}
		}
	}

	// Create the modal with improved form fields
	var blocks []slack.Block

	// Environment info section with summary
	var displayEnvName string
	if env, exists := status.Environments[environment]; exists && env.DisplayName != "" {
		displayEnvName = env.DisplayName
	} else {
		displayEnvName = strings.ReplaceAll(environment, "-", " ")
		displayEnvName = strings.Title(displayEnvName)
	}

	totalTags := len(tagOptions)
	occupiedCount := len(occupiedTags)

	// Check how many tags user already has assigned in this environment
	var userAssignedCount int
	var userAssignedTags []string
	if env, exists := status.Environments[environment]; exists {
		for tagName, tag := range env.Tags {
			if tag.Status == "occupied" && tag.AssignedTo == event.User.ID {
				userAssignedCount++
				displayTagName := strings.ReplaceAll(tagName, "-", " ")
				displayTagName = strings.Title(displayTagName)
				userAssignedTags = append(userAssignedTags, displayTagName)
			}
		}
	}

	infoText := fmt.Sprintf("*Environment:* %s\n", displayEnvName)
	infoText += fmt.Sprintf("🟢 *%d available* • 🔵 *%d in use by others* • 📋 *%d selectable*",
		availableCount, occupiedCount, totalTags)

	if userAssignedCount > 0 {
		infoText += fmt.Sprintf("\n📌 *You already have %d assigned:* %s", userAssignedCount, strings.Join(userAssignedTags, ", "))
	}

	if len(occupiedTags) > 0 {
		infoText += fmt.Sprintf("\n🔵 *In use by others:* %s", strings.Join(occupiedTags, ", "))
	}

	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", infoText, false, false),
		nil, nil,
	))

	// Tag multi-select (required)
	if len(tagOptions) > 0 {
		multiSelectElement := &slack.MultiSelectBlockElement{
			Type:        slack.MultiOptTypeStatic,
			ActionID:    "tag_select_action",
			Placeholder: slack.NewTextBlockObject("plain_text", "Choose one or more tags", false, false),
			Options:     tagOptions,
		}

		tagInputBlock := slack.NewInputBlock(
			"tag_select_block",
			slack.NewTextBlockObject("plain_text", "Tags *", false, false),
			slack.NewTextBlockObject("plain_text", "Select tags to reserve (available) or queue for (occupied)", false, false),
			multiSelectElement,
		)
		tagInputBlock.Optional = false // Required field
		blocks = append(blocks, tagInputBlock)
	} else {
		// No available tags - show message
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "❌ *No available tags* in this environment. All tags are currently occupied or in maintenance.", false, false),
			nil, nil,
		))
	}

	// Duration selection input - only show if there are available tags
	if len(tagOptions) > 0 {
		// Get configuration settings for duration bounds
		configSettings := app.ConfigService.GetSettings()
		minDuration := configSettings.MinDuration.ToDuration()
		maxDuration := configSettings.MaxDuration.ToDuration()

		// Generate duration options based on configuration (30-minute intervals)
		var durationOptions []*slack.OptionBlockObject
		for d := minDuration; d <= maxDuration; d += 30 * time.Minute {
			durationStr := d.String()
			// Convert Go duration format to more readable format
			readableText := ""
			hours := int(d.Hours())
			minutes := int(d.Minutes()) % 60

			if hours == 0 {
				readableText = fmt.Sprintf("%d minutes", minutes)
			} else if minutes == 0 {
				if hours == 1 {
					readableText = "1 hour"
				} else {
					readableText = fmt.Sprintf("%d hours", hours)
				}
			} else {
				if hours == 1 {
					readableText = fmt.Sprintf("1 hour %d minutes", minutes)
				} else {
					readableText = fmt.Sprintf("%d hours %d minutes", hours, minutes)
				}
			}

			durationOptions = append(durationOptions, slack.NewOptionBlockObject(
				durationStr,
				slack.NewTextBlockObject("plain_text", readableText, false, false),
				nil,
			))
		}

		durationElement := slack.NewOptionsSelectBlockElement(
			slack.OptTypeStatic,
			slack.NewTextBlockObject("plain_text", "Select duration", false, false),
			"duration_action",
			durationOptions...,
		)
		// Set default to smallest duration (first option)
		if len(durationOptions) > 0 {
			durationElement.InitialOption = durationOptions[0]
		}

		durationInputBlock := slack.NewInputBlock(
			"duration_input_block",
			slack.NewTextBlockObject("plain_text", "Duration *", false, false),
			slack.NewTextBlockObject("plain_text", "How long do you need the environment?", false, false),
			durationElement,
		)
		blocks = append(blocks, durationInputBlock)
	}

	// Create modal request
	var modalRequest slack.ModalViewRequest
	if len(tagOptions) > 0 {
		// Normal modal with submit button
		modalRequest = slack.ModalViewRequest{
			Type:       slack.ViewType("modal"),
			Title:      slack.NewTextBlockObject("plain_text", "Join Queue - Step 2", false, false),
			Close:      slack.NewTextBlockObject("plain_text", "Cancel", false, false),
			Submit:     slack.NewTextBlockObject("plain_text", "Join Queue", false, false),
			Blocks:     slack.Blocks{BlockSet: blocks},
			CallbackID: fmt.Sprintf("join_step2_%s", strings.ReplaceAll(environment, "-", "_")),
		}
	} else {
		// No available tags - modal with only close button
		modalRequest = slack.ModalViewRequest{
			Type:       slack.ViewType("modal"),
			Title:      slack.NewTextBlockObject("plain_text", "Join Queue - Step 2", false, false),
			Close:      slack.NewTextBlockObject("plain_text", "Close", false, false),
			Blocks:     slack.Blocks{BlockSet: blocks},
			CallbackID: fmt.Sprintf("join_step2_%s", strings.ReplaceAll(environment, "-", "_")),
		}
	}

	log.Printf("About to call UpdateView with ViewID: %s", event.View.ID)

	// Update the existing modal to step 2
	response, err := app.SlackClient.UpdateView(modalRequest, "", "", event.View.ID)
	if err != nil {
		log.Printf("Error opening step 2 modal: %v", err)
		log.Printf("Modal request: %+v", modalRequest)

		// Don't try to send message if channel ID is empty (modal interactions)
		if event.Channel.ID != "" {
			// Fallback to text message
			_, _, err = app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText(
				fmt.Sprintf("❌ Could not open modal for %s. Error: %v", environment, err), false))
		}
		return err
	}

	log.Printf("Successfully opened modal, response: %+v", response)
	return nil
}

// handleReleaseTagsModal opens a modal for releasing tags when user has multiple assignments
func (app *Application) handleReleaseTagsModal(event slack.InteractionCallback) error {
	log.Printf("Opening release tags modal for user %s", event.User.ID)

	// Get user's current assignments
	queueStatus := app.QueueService.GetQueueStatus()
	environments := queueStatus.Environments

	var userAssignments []*models.Tag
	for _, env := range environments {
		for _, tag := range env.Tags {
			if tag.AssignedTo == event.User.ID && tag.Status == "occupied" {
				userAssignments = append(userAssignments, tag)
			}
		}
	}

	if len(userAssignments) == 0 {
		// No assignments to release
		_, _, err := app.SlackClient.PostMessage(event.Channel.ID,
			slack.MsgOptionText("❌ You have no active assignments to release.", false))
		return err
	}

	if len(userAssignments) == 1 {
		// Only one assignment - release it directly
		assignment := userAssignments[0]
		releaseErr := app.QueueService.ReleaseSpecificTag(event.User.ID, assignment.Environment, assignment.Name)
		if releaseErr != nil {
			_, _, postErr := app.SlackClient.PostMessage(event.Channel.ID,
				slack.MsgOptionText(fmt.Sprintf("❌ Failed to release tag: %s", releaseErr.Error()), false))
			if postErr != nil {
				log.Printf("Error sending error message: %v", postErr)
			}
			return releaseErr
		}

		return nil
	}

	// Multiple assignments - show modal for selection
	var blocks []slack.Block

	// Header
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*🔓 Release Tags*\nYou have %d active assignments. Choose which ones to release:", len(userAssignments)), false, false),
		nil, nil,
	))

	// Add individual buttons for each tag assignment
	for _, assignment := range userAssignments {
		timeLeft := ""
		if !assignment.ExpiresAt.IsZero() {
			remaining := assignment.ExpiresAt.Sub(time.Now())
			if remaining > 0 {
				timeLeft = fmt.Sprintf("expires in %s", utils.FormatDuration(remaining))
			} else {
				timeLeft = "expired"
			}
		}

		// Create section with tag name and expiration info separated
		tagInfo := fmt.Sprintf("*%s/%s*", assignment.Environment, assignment.Name)
		if timeLeft != "" {
			tagInfo += fmt.Sprintf("\n⏰ %s", timeLeft)
		}

		tagButton := slack.NewButtonBlockElement(
			fmt.Sprintf("release_tag_%s_%s", assignment.Environment, assignment.Name),
			fmt.Sprintf("%s/%s", assignment.Environment, assignment.Name),
			slack.NewTextBlockObject("plain_text", "🔓 Release", false, false),
		)
		tagButton.Style = "danger"

		// Create section block with tag info and button as accessory
		tagSection := slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", tagInfo, false, false),
			nil,
			slack.NewAccessory(tagButton),
		)
		blocks = append(blocks, tagSection)
	}

	// Add divider
	blocks = append(blocks, slack.NewDividerBlock())

	// Add centered release all button
	releaseAllButton := slack.NewButtonBlockElement(
		"release_all_tags",
		"release_all",
		slack.NewTextBlockObject("plain_text", "🔥 Release All Tags", false, false),
	)
	releaseAllButton.Style = "danger"

	// Center the button by using action block
	centerActionBlock := slack.NewActionBlock("", releaseAllButton)
	blocks = append(blocks, centerActionBlock)

	modalRequest := slack.ModalViewRequest{
		Type:       slack.ViewType("modal"),
		Title:      slack.NewTextBlockObject("plain_text", "Release Tags", false, false),
		Close:      slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		Blocks:     slack.Blocks{BlockSet: blocks},
		CallbackID: "release_tags_modal",
	}

	// Open the modal
	_, openErr := app.SlackClient.OpenView(event.TriggerID, modalRequest)
	if openErr != nil {
		log.Printf("Error opening release tags modal: %v", openErr)
		_, _, postErr := app.SlackClient.PostMessage(event.Channel.ID,
			slack.MsgOptionText("❌ Could not open release modal.", false))
		if postErr != nil {
			log.Printf("Error sending fallback message: %v", postErr)
		}
		return openErr
	}

	log.Printf("Successfully opened release tags modal for user %s", event.User.ID)
	return nil
}

// handleReleaseTagsModalSubmission processes the release tags modal form data
func (app *Application) handleReleaseTagsModalSubmission(event slack.InteractionCallback) error {
	log.Printf("Processing release tags modal submission for user %s", event.User.ID)

	// Extract form values from the modal state
	if event.View.State == nil {
		_, _, err := app.SlackClient.PostMessage(event.User.ID,
			slack.MsgOptionText("❌ Could not access form data.", false))
		return err
	}
	values := event.View.State.Values

	// Get selected tags to release
	var tagsToRelease []string
	if releaseBlock, exists := values["release_tags_block"]; exists {
		if releaseValue, exists := releaseBlock["release_tags_select"]; exists {
			if releaseValue.SelectedOptions != nil {
				for _, option := range releaseValue.SelectedOptions {
					tagsToRelease = append(tagsToRelease, option.Value)
				}
			}
		}
	}

	if len(tagsToRelease) == 0 {
		_, _, err := app.SlackClient.PostMessage(event.User.ID,
			slack.MsgOptionText("❌ No tags selected for release.", false))
		return err
	}

	// Release each selected tag
	var releasedTags []string
	var errors []string

	for _, tagSpec := range tagsToRelease {
		// Parse environment/tag format
		parts := strings.Split(tagSpec, "/")
		if len(parts) != 2 {
			errors = append(errors, fmt.Sprintf("invalid tag format: %s", tagSpec))
			continue
		}

		environment := parts[0]
		tagName := parts[1]

		err := app.QueueService.ReleaseSpecificTag(event.User.ID, environment, tagName)
		if err != nil {
			errors = append(errors, fmt.Sprintf("failed to release %s: %s", tagSpec, err.Error()))
		} else {
			releasedTags = append(releasedTags, tagSpec)
		}
	}

	// Send result message
	if len(releasedTags) > 0 {
		message := fmt.Sprintf("✅ Successfully released %d tag(s): %s",
			len(releasedTags), strings.Join(releasedTags, ", "))
		if len(errors) > 0 {
			message += fmt.Sprintf("\n⚠️ Some releases failed: %s", strings.Join(errors, ", "))
		}
		_, _, err := app.SlackClient.PostMessage(app.ChannelID, slack.MsgOptionText(message, false))
		if err != nil {
			log.Printf("Error sending release success message: %v", err)
		}
	} else {
		message := fmt.Sprintf("❌ Failed to release any tags: %s", strings.Join(errors, ", "))
		_, _, err := app.SlackClient.PostMessage(event.User.ID, slack.MsgOptionText(message, false))
		if err != nil {
			log.Printf("Error sending release error message: %v", err)
		}
	}

	// Auto-update status after tag releases (silent update only)
	err := app.NotificationService.UpdateExistingQueueStatus()
	if err != nil {
		log.Printf("Error silently updating queue status after release all: %v", err)
	}

	return nil
}

// handleReleaseSelectedTags processes the release selected tags button click
func (app *Application) handleReleaseSelectedTags(event slack.InteractionCallback) error {
	log.Printf("Processing release selected tags button click for user %s", event.User.ID)

	// Extract form values from the modal state
	if event.View.State == nil {
		_, _, err := app.SlackClient.PostMessage(event.User.ID,
			slack.MsgOptionText("❌ Could not access form data.", false))
		return err
	}
	values := event.View.State.Values

	// Get selected tags to release
	var tagsToRelease []string
	if releaseBlock, exists := values["release_tags_block"]; exists {
		if releaseValue, exists := releaseBlock["release_tags_select"]; exists {
			if releaseValue.SelectedOptions != nil {
				for _, option := range releaseValue.SelectedOptions {
					tagsToRelease = append(tagsToRelease, option.Value)
				}
			}
		}
	}

	if len(tagsToRelease) == 0 {
		_, _, err := app.SlackClient.PostMessage(event.User.ID,
			slack.MsgOptionText("❌ No tags selected for release.", false))
		return err
	}

	// Release each selected tag
	var releasedTags []string
	var errors []string

	for _, tagSpec := range tagsToRelease {
		// Parse environment/tag format
		parts := strings.Split(tagSpec, "/")
		if len(parts) != 2 {
			errors = append(errors, fmt.Sprintf("invalid tag format: %s", tagSpec))
			continue
		}

		environment := parts[0]
		tagName := parts[1]

		err := app.QueueService.ReleaseSpecificTag(event.User.ID, environment, tagName)
		if err != nil {
			errors = append(errors, fmt.Sprintf("failed to release %s: %s", tagSpec, err.Error()))
		} else {
			releasedTags = append(releasedTags, tagSpec)
		}
	}

	// Send result message
	if len(releasedTags) > 0 {
		message := fmt.Sprintf("✅ <@%s> released %d tag(s): %s",
			event.User.ID, len(releasedTags), strings.Join(releasedTags, ", "))
		if len(errors) > 0 {
			message += fmt.Sprintf("\n⚠️ Some releases failed: %s", strings.Join(errors, ", "))
		}
		_, _, err := app.SlackClient.PostMessage(app.ChannelID, slack.MsgOptionText(message, false))
		if err != nil {
			log.Printf("Error sending release success message: %v", err)
		}
	} else {
		message := fmt.Sprintf("❌ Failed to release any tags: %s", strings.Join(errors, ", "))
		_, _, err := app.SlackClient.PostMessage(event.User.ID, slack.MsgOptionText(message, false))
		if err != nil {
			log.Printf("Error sending release error message: %v", err)
		}
	}

	return nil
}

// handleReleaseIndividualTag processes individual tag release button clicks
func (app *Application) handleReleaseIndividualTag(event slack.InteractionCallback, actionID string) error {
	log.Printf("Processing individual tag release for user %s, actionID %s", event.User.ID, actionID)

	// Parse the action ID to get environment and tag name
	// Format: "release_tag_ENVIRONMENT_TAGNAME"
	parts := strings.Split(actionID, "_")
	if len(parts) < 3 {
		_, _, err := app.SlackClient.PostMessage(event.User.ID,
			slack.MsgOptionText("❌ Invalid tag release request format.", false))
		return err
	}

	// Reconstruct environment and tag name (handling cases with underscores)
	environment := parts[2]
	tagName := strings.Join(parts[3:], "_")

	// Release the specific tag
	err := app.QueueService.ReleaseSpecificTag(event.User.ID, environment, tagName)
	if err != nil {
		_, _, postErr := app.SlackClient.PostMessage(event.User.ID,
			slack.MsgOptionText(fmt.Sprintf("❌ Failed to release %s/%s: %s", environment, tagName, err.Error()), false))
		if postErr != nil {
			log.Printf("Error sending error message: %v", postErr)
		}
		return err
	}

	// Success message is already sent by ReleaseSpecificTag via notification service

	// Auto-update status after individual tag release
	err = app.NotificationService.BroadcastOrUpdateQueueStatus()
	if err != nil {
		log.Printf("Error auto-updating queue status after individual tag release: %v", err)
	}

	// Refresh the modal to show updated state
	return app.refreshReleaseTagsModal(event)
}

// refreshReleaseTagsModal refreshes the release tags modal with fresh data
func (app *Application) refreshReleaseTagsModal(event slack.InteractionCallback) error {
	log.Printf("Refreshing release tags modal for user %s", event.User.ID)

	// Get user's current assignments
	queueStatus := app.QueueService.GetQueueStatus()
	environments := queueStatus.Environments

	var userAssignments []*models.Tag
	for _, env := range environments {
		for _, tag := range env.Tags {
			if tag.AssignedTo == event.User.ID && tag.Status == "occupied" {
				userAssignments = append(userAssignments, tag)
			}
		}
	}

	if len(userAssignments) == 0 {
		// No assignments left - show completion message
		noAssignmentsModal := slack.ModalViewRequest{
			Type:  slack.ViewType("modal"),
			Title: slack.NewTextBlockObject("plain_text", "Release Tags", false, false),
			Close: slack.NewTextBlockObject("plain_text", "Close", false, false),
			Blocks: slack.Blocks{BlockSet: []slack.Block{
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn", "✅ *All tags released!*\nYou have no active assignments to release.", false, false),
					nil, nil,
				),
			}},
		}
		_, err := app.SlackClient.UpdateView(noAssignmentsModal, "", "", event.View.ID)
		return err
	}

	// Multiple assignments - show modal for selection
	var blocks []slack.Block

	// Header
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*🔓 Release Tags*\nYou have %d active assignments. Choose which ones to release:", len(userAssignments)), false, false),
		nil, nil,
	))

	// Add individual buttons for each tag assignment
	for _, assignment := range userAssignments {
		timeLeft := ""
		if !assignment.ExpiresAt.IsZero() {
			remaining := assignment.ExpiresAt.Sub(time.Now())
			if remaining > 0 {
				timeLeft = fmt.Sprintf("expires in %s", utils.FormatDuration(remaining))
			} else {
				timeLeft = "expired"
			}
		}

		// Create section with tag name and expiration info separated
		tagInfo := fmt.Sprintf("*%s/%s*", assignment.Environment, assignment.Name)
		if timeLeft != "" {
			tagInfo += fmt.Sprintf("\n⏰ %s", timeLeft)
		}

		tagButton := slack.NewButtonBlockElement(
			fmt.Sprintf("release_tag_%s_%s", assignment.Environment, assignment.Name),
			fmt.Sprintf("%s/%s", assignment.Environment, assignment.Name),
			slack.NewTextBlockObject("plain_text", "🔓 Release", false, false),
		)
		tagButton.Style = "danger"

		// Create section block with tag info and button as accessory
		tagSection := slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", tagInfo, false, false),
			nil,
			slack.NewAccessory(tagButton),
		)
		blocks = append(blocks, tagSection)
	}

	// Add divider
	blocks = append(blocks, slack.NewDividerBlock())

	// Add centered release all button
	releaseAllButton := slack.NewButtonBlockElement(
		"release_all_tags",
		"release_all",
		slack.NewTextBlockObject("plain_text", "🔥 Release All Tags", false, false),
	)
	releaseAllButton.Style = "danger"

	// Center the button by using action block
	centerActionBlock := slack.NewActionBlock("", releaseAllButton)
	blocks = append(blocks, centerActionBlock)

	modalRequest := slack.ModalViewRequest{
		Type:       slack.ViewType("modal"),
		Title:      slack.NewTextBlockObject("plain_text", "Release Tags", false, false),
		Close:      slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		Blocks:     slack.Blocks{BlockSet: blocks},
		CallbackID: "release_tags_modal",
	}

	// Update the existing modal with fresh data
	_, err := app.SlackClient.UpdateView(modalRequest, "", "", event.View.ID)
	if err != nil {
		log.Printf("Error updating release tags modal: %v", err)
		return err
	}

	log.Printf("Successfully refreshed release tags modal for user %s", event.User.ID)
	return nil
}

// showReleaseAllCompletionModal updates the modal to show that all tags have been released
func (app *Application) showReleaseAllCompletionModal(event slack.InteractionCallback, releasedTags []string) error {
	log.Printf("Showing release all completion modal for user %s", event.User.ID)

	// Create completion message with released tags
	completionText := "✅ *All tags released!*\n\nYou have successfully released all your assignments"
	if len(releasedTags) > 0 {
		completionText += fmt.Sprintf(":\n\n• %s", strings.Join(releasedTags, "\n• "))
	}
	completionText += "\n\nAll tags are now available for other users to reserve."

	// Create modal with only the completion message
	completionModal := slack.ModalViewRequest{
		Type:  slack.ViewType("modal"),
		Title: slack.NewTextBlockObject("plain_text", "Release Tags - Complete", false, false),
		Close: slack.NewTextBlockObject("plain_text", "Close", false, false),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", completionText, false, false),
				nil, nil,
			),
		}},
	}

	// Update the existing modal
	_, err := app.SlackClient.UpdateView(completionModal, "", "", event.View.ID)
	if err != nil {
		log.Printf("Error updating modal to show completion: %v", err)
		return err
	}

	log.Printf("Successfully updated modal to show completion for user %s", event.User.ID)
	return nil
}

// handleManageAllModal opens a modal for users who have both assignments and queue positions
func (app *Application) handleManageAllModal(event slack.InteractionCallback) error {
	log.Printf("Opening manage all modal for user %s", event.User.ID)

	// Get user's current status
	queueStatus := app.QueueService.GetQueueStatus()
	var userAssignments []*models.Tag
	var userQueueItems []models.QueueItem

	// Get user's assignments
	for _, env := range queueStatus.Environments {
		for _, tag := range env.Tags {
			if tag.AssignedTo == event.User.ID && tag.Status == "occupied" {
				userAssignments = append(userAssignments, tag)
			}
		}
	}

	// Get user's queue positions
	for _, queueItem := range queueStatus.Queue {
		if queueItem.UserID == event.User.ID {
			userQueueItems = append(userQueueItems, queueItem)
		}
	}

	// Create modal blocks
	var blocks []slack.Block

	// Header
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", "*⚙️ Manage All*\nYou have both assignments and queue positions. Choose what you'd like to do:", false, false),
		nil, nil,
	))

	// Add assignment section if user has assignments
	if len(userAssignments) > 0 {
		assignmentText := "*🎉 Your Active Assignments:*\n"
		for _, tag := range userAssignments {
			timeLeft := tag.ExpiresAt.Sub(time.Now())
			assignmentText += fmt.Sprintf("• %s/%s (expires in %s)\n", tag.Environment, tag.Name, utils.FormatDuration(timeLeft))
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", assignmentText, false, false),
			nil, nil,
		))
	}

	// Add queue section if user is in queue
	if len(userQueueItems) > 0 {
		queueText := "*⏳ Your Queue Positions:*\n"
		for i, item := range userQueueItems {
			queueText += fmt.Sprintf("• %s/%s (position %d)\n", item.Environment, item.Tag, i+1)
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", queueText, false, false),
			nil, nil,
		))
	}

	// Add action buttons
	var actionElements []slack.BlockElement

	// Release all tags button
	if len(userAssignments) > 0 {
		releaseAllButton := slack.NewButtonBlockElement("release_all_tags", "release_all_tags",
			slack.NewTextBlockObject("plain_text", "🔓 Release All Tags", true, false))
		releaseAllButton.Style = "danger"
		actionElements = append(actionElements, releaseAllButton)
	}

	// Leave all queues button
	if len(userQueueItems) > 0 {
		leaveAllButton := slack.NewButtonBlockElement("leave_all_queues", "leave_all_queues",
			slack.NewTextBlockObject("plain_text", "🚪 Leave All Queues", true, false))
		actionElements = append(actionElements, leaveAllButton)
	}

	// Abandon all button (both release tags and leave queues)
	if len(userAssignments) > 0 && len(userQueueItems) > 0 {
		abandonAllButton := slack.NewButtonBlockElement("abandon_all", "abandon_all",
			slack.NewTextBlockObject("plain_text", "💥 Abandon All", true, false))
		abandonAllButton.Style = "danger"
		actionElements = append(actionElements, abandonAllButton)
	}

	// Individual management buttons
	if len(userAssignments) > 0 {
		manageTagsButton := slack.NewButtonBlockElement("release_tags", "release_tags",
			slack.NewTextBlockObject("plain_text", "🔓 Manage Tags", true, false))
		actionElements = append(actionElements, manageTagsButton)
	}

	// Add action block
	actionBlock := slack.NewActionBlock("manage_actions", actionElements...)
	blocks = append(blocks, actionBlock)

	// Create modal
	modalRequest := slack.ModalViewRequest{
		Type:   slack.ViewType("modal"),
		Title:  slack.NewTextBlockObject("plain_text", "Manage All", false, false),
		Close:  slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		Blocks: slack.Blocks{BlockSet: blocks},
	}

	// Open modal
	_, err := app.SlackClient.OpenView(event.TriggerID, modalRequest)
	if err != nil {
		log.Printf("Error opening manage all modal: %v", err)
		return err
	}

	log.Printf("Successfully opened manage all modal for user %s", event.User.ID)
	return nil
}

// handleLeaveMultipleQueuesModal opens a modal for leaving multiple queues with selection
func (app *Application) handleLeaveMultipleQueuesModal(event slack.InteractionCallback, queueItems []models.QueueItem) error {
	log.Printf("Opening leave multiple queues modal for user %s, %d queue items", event.User.ID, len(queueItems))

	// Create modal blocks
	var blocks []slack.Block

	// Header
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", "*🚪 Leave Queues*\nSelect which queues you want to leave:", false, false),
		nil, nil,
	))

	// Add queue items as individual sections
	for _, queueItem := range queueItems {
		queueText := fmt.Sprintf("*%s/%s*\nDuration: %s | Joined: %s",
			queueItem.Environment, queueItem.Tag,
			utils.FormatDuration(queueItem.Duration),
			queueItem.JoinedAt.Format("15:04"))

		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", queueText, false, false),
			nil, nil,
		))
	}

	// Add action buttons
	var actionElements []slack.BlockElement

	// Leave selected button
	leaveSelectedButton := slack.NewButtonBlockElement("leave_selected_queues", "leave_selected_queues",
		slack.NewTextBlockObject("plain_text", "🚪 Leave Selected", true, false))
	leaveSelectedButton.Style = "danger"
	actionElements = append(actionElements, leaveSelectedButton)

	// Leave all button
	leaveAllButton := slack.NewButtonBlockElement("leave_all_queues", "leave_all_queues",
		slack.NewTextBlockObject("plain_text", "🚪 Leave All", true, false))
	leaveAllButton.Style = "danger"
	actionElements = append(actionElements, leaveAllButton)

	// Cancel button
	cancelButton := slack.NewButtonBlockElement("cancel_leave_queues", "cancel_leave_queues",
		slack.NewTextBlockObject("plain_text", "Cancel", true, false))
	actionElements = append(actionElements, cancelButton)

	// Add action block
	actionBlock := slack.NewActionBlock("leave_queues_actions", actionElements...)
	blocks = append(blocks, actionBlock)

	// Create modal
	modalRequest := slack.ModalViewRequest{
		Type:   slack.ViewType("modal"),
		Title:  slack.NewTextBlockObject("plain_text", "Leave Queues", false, false),
		Close:  slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		Blocks: slack.Blocks{BlockSet: blocks},
	}

	// Open modal
	_, err := app.SlackClient.OpenView(event.TriggerID, modalRequest)
	if err != nil {
		log.Printf("Error opening leave multiple queues modal: %v", err)
		return err
	}

	log.Printf("Successfully opened leave multiple queues modal for user %s", event.User.ID)
	return nil
}

// closeModal closes a modal by sending an empty response
func (app *Application) closeModal(event slack.InteractionCallback) error {
	// The modal will close automatically when the user clicks a button
	log.Printf("Modal would be closed for user %s", event.User.ID)
	return nil
}

// startHTTPServer starts the HTTP server for health checks and API endpoints
func (app *Application) startHTTPServer() error {
	// Health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		status, message := app.HTTPHandler.HealthCheck()
		w.WriteHeader(status)
		w.Write([]byte(message))
	})

	// API endpoints
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		status, data := app.HTTPHandler.GetStatus()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(data)
	})

	http.HandleFunc("/api/save", func(w http.ResponseWriter, r *http.Request) {
		status, message := app.HTTPHandler.ManualSave()
		w.WriteHeader(status)
		w.Write([]byte(message))
	})

	log.Println("Starting HTTP server on :8080")
	return http.ListenAndServe(":8080", nil)
}

func main() {
	// Create application with dependency injection
	app, err := NewApplication()
	if err != nil {
		log.Fatalf("Failed to create application: %v", err)
	}

	// Ensure cleanup happens
	defer func() {
		if err := app.Stop(); err != nil {
			log.Printf("Error during shutdown: %v", err)
		}
	}()

	// Run the application
	log.Println("Starting Slack Queue Bot...")
	if err := app.Start(); err != nil {
		log.Fatalf("Application failed: %v", err)
	}
}
