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
	ConfigService       interfaces.ConfigService
	ValidationService   interfaces.ValidationService
	QueueService        interfaces.QueueService
	TagService          interfaces.TagService
	NotificationService interfaces.NotificationService
	ExpirationService   *services.ExpirationService

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

	config := configManager.Get()

	// Create repository wrapper for database operations
	repository := database.NewRepository(db)

	// Create services with dependency injection
	validationService := services.NewValidationService(&config.Settings)
	notificationService := services.NewNotificationService(slackClient, channelID, &config.Settings)
	queueService := services.NewQueueService(repository, validationService, notificationService, configManager)
	tagService := services.NewTagService(repository, validationService, notificationService, configManager)

	// Set queue service reference in notification service (to break circular dependency)
	notificationService.SetQueueService(queueService)

	// Create expiration service for real-time database notifications
	expirationService := services.NewExpirationService(repository, notificationService, queueService)

	// Create handlers
	slackHandler := handlers.NewSlackHandler(slackClient, queueService, tagService, notificationService, validationService, &config.Settings)
	httpHandler := handlers.NewHTTPHandler(queueService, configManager, tagService)

	// Create context for application lifecycle
	ctx, cancel := context.WithCancel(context.Background())

	return &Application{
		SlackClient:         slackClient,
		SocketClient:        socketClient,
		ChannelID:           channelID,
		DataDir:             dataDir,
		ConfigPath:          configPath,
		DB:                  db,
		ctx:                 ctx,
		cancel:              cancel,
		ConfigService:       configManager,
		ValidationService:   validationService,
		QueueService:        queueService,
		TagService:          tagService,
		NotificationService: notificationService,
		ExpirationService:   expirationService,
		SlackHandler:        slackHandler,
		HTTPHandler:         httpHandler,
	}, nil
}

// Start starts the application
func (app *Application) Start() error {
	// Start the expiration service
	log.Println("Starting expiration service...")
	dbPath := fmt.Sprintf("%s/queue.db", app.DataDir)
	if err := app.ExpirationService.StartExpirationListener(dbPath); err != nil {
		log.Printf("Warning: Failed to start expiration service: %v", err)
	}

	// Clean up old auto-expiration log entries to prevent duplicate notifications on restart
	log.Println("Cleaning up old auto-expiration log entries...")
	if err := app.DB.CleanupAutoExpirationLogs(24 * time.Hour); err != nil {
		log.Printf("Warning: Failed to cleanup auto-expiration logs: %v", err)
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
	if app.ExpirationService != nil {
		app.ExpirationService.Stop()
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
						// Special handling for status command - send blocks instead of text
						if response.Message == "status" && response.IsEphemeral {
							// Get status blocks from notification service
							blocks, err := app.NotificationService.CreateQueueStatusBlocks()
							if err != nil {
								log.Printf("Error getting status blocks: %v", err)
								// Fallback to error message
								_, err := app.SlackClient.PostEphemeral(ev.Channel, ev.User, slack.MsgOptionText("❌ Failed to get status", false))
								if err != nil {
									log.Printf("Error sending ephemeral error: %v", err)
								}
							} else {
								// Send status blocks as ephemeral message
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

	// For now, use first selected tag (can be enhanced later for multiple tags)
	var tag string
	if len(tags) > 0 {
		tag = tags[0]
	}

	// Fallback to old single tag input methods for backward compatibility
	if tag == "" {
		if tagBlock, exists := values["tag_input_block"]; exists {
			if tagValue, exists := tagBlock["tag_action"]; exists {
				tag = strings.TrimSpace(tagValue.Value)
			}
		}
		if tag == "" {
			if tagBlock, exists := values["tag_input"]; exists {
				if tagValue, exists := tagBlock["tag"]; exists {
					tag = strings.TrimSpace(tagValue.Value)
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

	// Join the queue
	err = app.QueueService.JoinQueue(event.User.ID, user.Name, environment, tag, duration)
	if err != nil {
		log.Printf("Error joining queue: %v", err)
		// Send error response to user via DM
		_, _, _ = app.SlackClient.PostMessage(event.User.ID, slack.MsgOptionText(
			fmt.Sprintf("❌ Failed to join queue: %s", err.Error()), false))
		return err
	}

	log.Printf("Successfully added user %s to queue for %s/%s (%s)", event.User.ID, environment, tag, duration)

	// Send success message to the configured channel (modal interactions don't have channel ID)
	var message string
	displayEnvName := strings.ReplaceAll(environment, "-", " ")
	displayEnvName = strings.Title(displayEnvName)

	if tag != "" {
		displayTagName := strings.ReplaceAll(tag, "-", " ")
		displayTagName = strings.Title(displayTagName)
		message = fmt.Sprintf("✅ <@%s> joined the queue for *%s / %s* (%s)",
			event.User.ID, displayEnvName, displayTagName, utils.FormatDuration(duration))
	} else {
		message = fmt.Sprintf("✅ <@%s> joined the queue for *%s* (%s)",
			event.User.ID, displayEnvName, utils.FormatDuration(duration))
	}

	_, _, err = app.SlackClient.PostMessage(app.ChannelID, slack.MsgOptionText(message, false))
	if err != nil {
		log.Printf("Error sending success message: %v", err)
	} else {
		log.Printf("Successfully sent success message to channel %s", app.ChannelID)
	}

	return nil
}

// handleJoinQueueModal opens the first step modal for environment selection
func (app *Application) handleJoinQueueModal(event slack.InteractionCallback) error {
	log.Printf("Opening join queue modal (step 1) for user %s", event.User.ID)

	// Get environments from queue service
	status := app.QueueService.GetQueueStatus()

	// Convert map to slice and sort environments by name
	var envList []*models.Environment
	for _, env := range status.Environments {
		envList = append(envList, env)
	}
	sort.Slice(envList, func(i, j int) bool {
		return envList[i].Name < envList[j].Name
	})

	// Create action blocks with buttons for each environment in 2x2 grid
	var blocks []slack.Block

	// Add header
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", "*Select an environment:*", false, false),
		nil, nil,
	))

	// Create buttons in rows of 2
	var actionElements []slack.BlockElement
	for i, env := range envList {
		availableCount := 0
		for _, tag := range env.Tags {
			if tag.Status == "available" {
				availableCount++
			}
		}

		// Create more readable display name
		displayName := strings.ReplaceAll(env.Name, "-", " ")
		displayName = strings.Title(displayName)
		buttonText := fmt.Sprintf("%s (%d available)", displayName, availableCount)

		// Make button ID unique by including index
		button := slack.NewButtonBlockElement(
			fmt.Sprintf("select_env_%s_%d", env.Name, i),
			env.Name,
			slack.NewTextBlockObject("plain_text", buttonText, false, false),
		)
		actionElements = append(actionElements, button)

		// Add action block every 2 buttons or at the end
		if len(actionElements) == 2 || i == len(envList)-1 {
			// Make block ID unique
			blockID := fmt.Sprintf("env_row_%d", i/2)
			blocks = append(blocks, slack.NewActionBlock(blockID, actionElements...))
			actionElements = []slack.BlockElement{} // Reset for next row
		}
	}

	modalRequest := slack.ModalViewRequest{
		Type:       slack.ViewType("modal"),
		Title:      slack.NewTextBlockObject("plain_text", "Join Queue - Step 1", false, false),
		Close:      slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		Blocks:     slack.Blocks{BlockSet: blocks},
		CallbackID: "join_queue_step1_modal",
	}

	// Open the modal
	_, err := app.SlackClient.OpenView(event.TriggerID, modalRequest)
	if err != nil {
		log.Printf("Error opening modal: %v", err)
		// Fallback to text message
		_, _, err = app.SlackClient.PostMessage(event.Channel.ID, slack.MsgOptionText(
			"❌ Could not open modal. Please use: `@bot join <environment> [tag] [duration]`", false))
		return err
	}

	log.Printf("Successfully opened join queue modal (step 1) for user %s", event.User.ID)
	return nil
}

// handleJoinQueueStep2Modal opens the second step modal for tag and duration selection
func (app *Application) handleJoinQueueStep2Modal(event slack.InteractionCallback, environment string) error {
	log.Printf("Opening join queue modal (step 2) for user %s, environment %s", event.User.ID, environment)

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
			if tag.Status == "available" {
				availableCount++
				// Create more readable tag display name
				displayTagName := strings.ReplaceAll(tagName, "-", " ")
				displayTagName = strings.Title(displayTagName)

				// Add available tags to multi-select options
				tagOptions = append(tagOptions, slack.NewOptionBlockObject(
					tagName,
					slack.NewTextBlockObject("plain_text", displayTagName, false, false),
					slack.NewTextBlockObject("plain_text", "🟢 available", false, false),
				))
			} else if tag.Status == "occupied" {
				displayTagName := strings.ReplaceAll(tagName, "-", " ")
				displayTagName = strings.Title(displayTagName)
				occupiedTags = append(occupiedTags, displayTagName)
			}
		}
	}

	// Create the modal with improved form fields
	var blocks []slack.Block

	// Environment info section with summary
	displayEnvName := strings.ReplaceAll(environment, "-", " ")
	displayEnvName = strings.Title(displayEnvName)

	infoText := fmt.Sprintf("*Environment:* %s\n", displayEnvName)
	infoText += fmt.Sprintf("🟢 *%d available tags*", availableCount)
	if len(occupiedTags) > 0 {
		infoText += fmt.Sprintf(" • 🔴 *Occupied:* %s", strings.Join(occupiedTags, ", "))
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
			slack.NewTextBlockObject("plain_text", "Select the tags you want to reserve", false, false),
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
