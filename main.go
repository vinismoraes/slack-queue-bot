package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// Tag represents a bookable tag within an environment
type Tag struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"` // "available", "occupied", "maintenance"
	AssignedTo  string    `json:"assigned_to,omitempty"`
	AssignedAt  time.Time `json:"assigned_at,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	Environment string    `json:"environment"`
}

// Environment represents a test environment containing multiple tags
type Environment struct {
	Name string          `json:"name"`
	Tags map[string]*Tag `json:"tags"`
}

// QueueItem represents a user in the queue for a specific tag
type QueueItem struct {
	UserID      string        `json:"user_id"`
	Username    string        `json:"username"`
	JoinedAt    time.Time     `json:"joined_at"`
	Environment string        `json:"environment"`
	Tag         string        `json:"tag"`
	Duration    time.Duration `json:"duration"` // How long they need the tag for
}

// PersistenceData represents the complete state to be saved/loaded
type PersistenceData struct {
	Queue        []QueueItem             `json:"queue"`
	Environments map[string]*Environment `json:"environments"`
	LastSaved    time.Time               `json:"last_saved"`
}

// Config represents the configuration structure
type Config struct {
	Environments []struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	} `json:"environments"`
}

// QueueManager handles the queue and tag management
type QueueManager struct {
	queue        []QueueItem
	environments map[string]*Environment
	mutex        sync.RWMutex
	slackClient  *slack.Client
	channelID    string
	dataDir      string
	configPath   string
	lastSaved    time.Time
}

// NewQueueManager creates a new queue manager
func NewQueueManager(slackClient *slack.Client, channelID string, dataDir string, configPath string) *QueueManager {
	return &QueueManager{
		queue:        make([]QueueItem, 0),
		environments: make(map[string]*Environment),
		slackClient:  slackClient,
		channelID:    channelID,
		dataDir:      dataDir,
		configPath:   configPath,
		lastSaved:    time.Now(),
	}
}

// saveState saves the current state to JSON files
func (qm *QueueManager) saveState() error {
	qm.mutex.RLock()
	defer qm.mutex.RUnlock()

	// Create data directory if it doesn't exist
	if err := os.MkdirAll(qm.dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %v", err)
	}

	// Prepare data for saving
	data := PersistenceData{
		Queue:        make([]QueueItem, len(qm.queue)),
		Environments: make(map[string]*Environment),
		LastSaved:    time.Now(),
	}

	// Copy queue
	copy(data.Queue, qm.queue)

	// Copy environments
	for k, v := range qm.environments {
		data.Environments[k] = &Environment{
			Name: v.Name,
			Tags: make(map[string]*Tag),
		}
		for tagK, tagV := range v.Tags {
			data.Environments[k].Tags[tagK] = &Tag{
				Name:        tagV.Name,
				Status:      tagV.Status,
				AssignedTo:  tagV.AssignedTo,
				AssignedAt:  tagV.AssignedAt,
				ExpiresAt:   tagV.ExpiresAt,
				Environment: tagV.Environment,
			}
		}
	}

	// Save to file
	filePath := fmt.Sprintf("%s/queue_state.json", qm.dataDir)
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create state file: %v", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to encode state: %v", err)
	}

	qm.lastSaved = time.Now()
	log.Printf("State saved to %s", filePath)
	return nil
}

// loadState loads the state from JSON files
func (qm *QueueManager) loadState() error {
	filePath := fmt.Sprintf("%s/queue_state.json", qm.dataDir)

	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		log.Printf("No existing state file found at %s, starting fresh", filePath)
		return nil
	}

	// Read file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open state file: %v", err)
	}
	defer file.Close()

	var data PersistenceData
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&data); err != nil {
		return fmt.Errorf("failed to decode state: %v", err)
	}

	qm.mutex.Lock()
	defer qm.mutex.Unlock()

	// Load queue
	qm.queue = make([]QueueItem, len(data.Queue))
	copy(qm.queue, data.Queue)

	// Load environments
	qm.environments = make(map[string]*Environment)
	for k, v := range data.Environments {
		qm.environments[k] = &Environment{
			Name: v.Name,
			Tags: make(map[string]*Tag),
		}
		for tagK, tagV := range v.Tags {
			qm.environments[k].Tags[tagK] = &Tag{
				Name:        tagV.Name,
				Status:      tagV.Status,
				AssignedTo:  tagV.AssignedTo,
				AssignedAt:  tagV.AssignedAt,
				ExpiresAt:   tagV.ExpiresAt,
				Environment: tagV.Environment,
			}
		}
	}

	qm.lastSaved = data.LastSaved
	log.Printf("State loaded from %s (last saved: %s)", filePath, qm.lastSaved.Format(time.RFC3339))
	return nil
}

// autoSave triggers automatic saving of state
func (qm *QueueManager) autoSave() {
	if err := qm.saveState(); err != nil {
		log.Printf("Error auto-saving state: %v", err)
	}
}

// loadConfig loads the configuration from the config file
func (qm *QueueManager) loadConfig() error {
	file, err := os.Open(qm.configPath)
	if err != nil {
		return fmt.Errorf("failed to open config file: %v", err)
	}
	defer file.Close()

	var config Config
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("failed to decode config: %v", err)
	}

	return nil
}

// initializeEnvironments initializes environments from configuration
func (qm *QueueManager) initializeEnvironments() error {
	file, err := os.Open(qm.configPath)
	if err != nil {
		return fmt.Errorf("failed to open config file: %v", err)
	}
	defer file.Close()

	var config Config
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("failed to decode config: %v", err)
	}

	for _, envConfig := range config.Environments {
		env := &Environment{
			Name: envConfig.Name,
			Tags: make(map[string]*Tag),
		}

		for _, tagName := range envConfig.Tags {
			env.Tags[tagName] = &Tag{
				Name:        tagName,
				Status:      "available",
				Environment: envConfig.Name,
			}
		}

		qm.environments[envConfig.Name] = env
	}

	log.Printf("Initialized %d environments from configuration", len(config.Environments))
	return nil
}

// JoinQueue adds a user to the queue for a specific tag
func (qm *QueueManager) JoinQueue(userID, username, environment, tag string, duration time.Duration) error {
	qm.mutex.Lock()
	defer qm.mutex.Unlock()

	// Enforce queue limit
	maxQueueSize := 50 // default
	if configPath := qm.configPath; configPath != "" {
		file, err := os.Open(configPath)
		if err == nil {
			var config struct {
				Settings struct {
					MaxQueueSize int `json:"max_queue_size"`
				} `json:"settings"`
			}
			if err := json.NewDecoder(file).Decode(&config); err == nil && config.Settings.MaxQueueSize > 0 {
				maxQueueSize = config.Settings.MaxQueueSize
			}
			file.Close()
		}
	}
	if len(qm.queue) >= maxQueueSize {
		qm.notifyUserBlock(userID, "🚫", "Queue is Full!", fmt.Sprintf("The queue is currently at its limit (%d/%d).", len(qm.queue), maxQueueSize), "Please try again later or ask an admin for help.")
		return fmt.Errorf("queue is full")
	}

	// Check if user is already in queue
	for _, item := range qm.queue {
		if item.UserID == userID {
			qm.notifyUserBlock(userID, "ℹ️", "Already in Queue", "You are already in the queue.", "Use `@bot position` to check your spot or `@bot leave` to exit.")
			return fmt.Errorf("already in queue")
		}
	}

	// Validate duration (minimum 30 minutes, maximum 3 hours, 30-minute intervals)
	if duration < 30*time.Minute {
		qm.notifyUserBlock(userID, "⚠️", "Duration Too Short", "Minimum duration is 30 minutes.", "Please specify a longer duration.")
		return fmt.Errorf("duration too short")
	}
	if duration > 3*time.Hour {
		qm.notifyUserBlock(userID, "⚠️", "Duration Too Long", "Maximum duration is 3 hours.", "Please specify a shorter duration.")
		return fmt.Errorf("duration too long")
	}

	// Check if duration is in 30-minute intervals
	if duration%(30*time.Minute) != 0 {
		validOptions := strings.Join(getValidDurations(), ", ")
		qm.notifyUserBlock(userID, "⚠️", "Invalid Duration", "Duration must be in 30-minute intervals.", "Valid options: "+validOptions)
		return fmt.Errorf("invalid duration interval")
	}

	// Check if environment exists
	env, exists := qm.environments[environment]
	if !exists {
		qm.notifyUserBlock(userID, "❌", "Environment Not Found", fmt.Sprintf("Environment '%s' does not exist.", environment), "Use `@bot list` to see available environments.")
		return fmt.Errorf("environment not found")
	}

	// Check if tag exists and is available
	if tag != "" {
		tagObj, tagExists := env.Tags[tag]
		if !tagExists {
			qm.notifyUserBlock(userID, "❌", "Tag Not Found", fmt.Sprintf("Tag '%s' does not exist in environment '%s'.", tag, environment), "Use `@bot list` to see available tags.")
			return fmt.Errorf("tag not found")
		}
		if tagObj.Status != "available" {
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
			qm.notifyUserBlock(userID, "🔴", "Tag Unavailable", msg, suggestion)
			return fmt.Errorf("tag unavailable")
		}
	}

	queueItem := QueueItem{
		UserID:      userID,
		Username:    username,
		JoinedAt:    time.Now(),
		Environment: environment,
		Tag:         tag,
		Duration:    duration,
	}

	qm.queue = append(qm.queue, queueItem)

	// Notify user with queue position
	positionInfo := qm.GetQueuePositionInfo(userID)
	qm.notifyUser(userID, positionInfo)

	qm.broadcastQueueUpdate()

	// Auto-save state
	go qm.autoSave()
	return nil
}

// LeaveQueue removes a user from the queue
func (qm *QueueManager) LeaveQueue(userID string) error {
	qm.mutex.Lock()
	defer qm.mutex.Unlock()

	for i, item := range qm.queue {
		if item.UserID == userID {
			// If user was assigned a tag, release it
			if item.Tag != "" && item.Environment != "" {
				if env, exists := qm.environments[item.Environment]; exists {
					if tag, tagExists := env.Tags[item.Tag]; tagExists {
						tag.Status = "available"
						tag.AssignedTo = ""
						tag.AssignedAt = time.Time{}
						tag.ExpiresAt = time.Time{}
					}
				}
			}

			// Remove from queue
			qm.queue = append(qm.queue[:i], qm.queue[i+1:]...)

			// Notify users who moved up in the queue
			qm.notifyQueuePositionChanges()

			qm.broadcastQueueUpdate()

			// Auto-save state
			go qm.autoSave()
			return nil
		}
	}

	return fmt.Errorf("you are not in the queue")
}

// notifyQueuePositionChanges notifies users when their queue position changes
func (qm *QueueManager) notifyQueuePositionChanges() {
	// This would be called after someone leaves the queue
	// For now, we'll just notify the first person in line if they're now next
	if len(qm.queue) > 0 {
		firstUser := qm.queue[0]
		qm.notifyUser(firstUser.UserID, "🎯 You're now next in line! You'll be assigned as soon as a tag becomes available.")
	}
}

// AssignNextUser assigns the next user in queue to an available tag
func (qm *QueueManager) AssignNextUser() error {
	qm.mutex.Lock()
	defer qm.mutex.Unlock()

	if len(qm.queue) == 0 {
		return fmt.Errorf("queue is empty")
	}

	// Find available tag
	var availableTag *Tag
	var availableEnv *Environment

	for _, env := range qm.environments {
		for _, tag := range env.Tags {
			if tag.Status == "available" {
				availableTag = tag
				availableEnv = env
				break
			}
		}
		if availableTag != nil {
			break
		}
	}

	if availableTag == nil {
		return fmt.Errorf("no tags available")
	}

	// Assign to first user in queue
	user := qm.queue[0]
	availableTag.Status = "occupied"
	availableTag.AssignedTo = user.UserID
	availableTag.AssignedAt = time.Now()
	availableTag.ExpiresAt = time.Now().Add(user.Duration) // Use user's requested duration

	// Remove user from queue
	qm.queue = qm.queue[1:]

	// Notify user with time limit info
	expiryTime := availableTag.ExpiresAt.Format("15:04")
	durationText := formatDuration(user.Duration)
	qm.notifyUser(user.UserID, fmt.Sprintf("🎉 You've been assigned to tag: *%s* in environment: *%s*\n⏰ This assignment expires at *%s* (%s from now)", availableTag.Name, availableEnv.Name, expiryTime, durationText))

	qm.broadcastQueueUpdate()

	// Auto-save state
	go qm.autoSave()
	return nil
}

// GetQueueStatus returns the current queue status
func (qm *QueueManager) GetQueueStatus() ([]QueueItem, map[string]*Environment) {
	qm.mutex.RLock()
	defer qm.mutex.RUnlock()

	queueCopy := make([]QueueItem, len(qm.queue))
	copy(queueCopy, qm.queue)

	envCopy := make(map[string]*Environment)
	for k, v := range qm.environments {
		envCopy[k] = &Environment{
			Name: v.Name,
			Tags: make(map[string]*Tag),
		}
		for tagK, tagV := range v.Tags {
			envCopy[k].Tags[tagK] = &Tag{
				Name:        tagV.Name,
				Status:      tagV.Status,
				AssignedTo:  tagV.AssignedTo,
				AssignedAt:  tagV.AssignedAt,
				ExpiresAt:   tagV.ExpiresAt,
				Environment: tagV.Environment,
			}
		}
	}

	return queueCopy, envCopy
}

// GetUserQueuePosition returns the position of a user in the queue (1-based index)
// Returns 0 if user is not in queue, -1 if user has an active assignment
func (qm *QueueManager) GetUserQueuePosition(userID string) (int, *QueueItem) {
	qm.mutex.RLock()
	defer qm.mutex.RUnlock()

	// Check if user has an active assignment
	for _, env := range qm.environments {
		for _, tag := range env.Tags {
			if tag.AssignedTo == userID && tag.Status == "occupied" {
				return -1, nil // User has an active assignment
			}
		}
	}

	// Check queue position
	for i, item := range qm.queue {
		if item.UserID == userID {
			return i + 1, &item // Return 1-based position
		}
	}

	return 0, nil // User not in queue
}

// GetQueuePositionInfo returns detailed information about a user's queue position
func (qm *QueueManager) GetQueuePositionInfo(userID string) string {
	position, item := qm.GetUserQueuePosition(userID)

	if position == -1 {
		// User has an active assignment
		for _, env := range qm.environments {
			for _, tag := range env.Tags {
				if tag.AssignedTo == userID && tag.Status == "occupied" {
					timeLeft := tag.ExpiresAt.Sub(time.Now())
					if timeLeft > 0 {
						return fmt.Sprintf("🎉 You have an active assignment to *%s* in *%s* (expires in %s)",
							tag.Name, env.Name, formatDuration(timeLeft))
					} else {
						return "⚠️ Your assignment has expired and will be released soon"
					}
				}
			}
		}
		return "🎉 You have an active assignment"
	}

	if position == 0 {
		return "❌ You are not in the queue"
	}

	// User is in queue
	queue, _ := qm.GetQueueStatus()
	totalInQueue := len(queue)

	// Calculate estimated wait time (rough estimate: 30 minutes per person ahead)
	estimatedWaitMinutes := (position - 1) * 30
	estimatedWait := time.Duration(estimatedWaitMinutes) * time.Minute

	// Get available tags count
	availableTags := 0
	for _, env := range qm.environments {
		for _, tag := range env.Tags {
			if tag.Status == "available" {
				availableTags++
			}
		}
	}

	positionText := fmt.Sprintf("📋 You are *position %d* of %d in the queue", position, totalInQueue)

	if item != nil {
		waitText := ""
		if position > 1 {
			waitText = fmt.Sprintf("\n⏱️ Estimated wait time: *%s*", formatDuration(estimatedWait))
		} else {
			waitText = "\n🎯 You're next in line!"
		}

		requestText := ""
		if item.Tag != "" {
			requestText = fmt.Sprintf("\n🎯 Waiting for: *%s/%s* for *%s*", item.Environment, item.Tag, formatDuration(item.Duration))
		} else if item.Environment != "" {
			requestText = fmt.Sprintf("\n🎯 Waiting for: *%s* for *%s*", item.Environment, formatDuration(item.Duration))
		}

		availableText := fmt.Sprintf("\n🟢 Available tags: *%d*", availableTags)

		return positionText + waitText + requestText + availableText
	}

	return positionText
}

// broadcastQueueUpdate sends queue status to the channel
func (qm *QueueManager) broadcastQueueUpdate() {
	queue, environments := qm.GetQueueStatus()

	maxQueueSize := 50 // default
	if configPath := qm.configPath; configPath != "" {
		file, err := os.Open(configPath)
		if err == nil {
			var config struct {
				Settings struct {
					MaxQueueSize int `json:"max_queue_size"`
				} `json:"settings"`
			}
			if err := json.NewDecoder(file).Decode(&config); err == nil && config.Settings.MaxQueueSize > 0 {
				maxQueueSize = config.Settings.MaxQueueSize
			}
			file.Close()
		}
	}

	blocks := []slack.Block{}

	// Show warning if queue is full or nearly full
	if len(queue) >= maxQueueSize {
		blocks = append(blocks, slack.NewContextBlock("queue_full_warning",
			slack.NewTextBlockObject("mrkdwn", ":warning: *Queue is full!* (%d/%d). Please try again later or ask an admin for help.", false, false),
		))
	} else if len(queue) >= maxQueueSize-2 {
		blocks = append(blocks, slack.NewContextBlock("queue_almost_full",
			slack.NewTextBlockObject("mrkdwn", ":warning: *Queue is almost full!* (%d/%d)", false, false),
		))
	}

	blocks = append(blocks, slack.NewHeaderBlock(slack.NewTextBlockObject("plain_text", "📋 Test Environment Queue", false, false)))

	// Queue section
	if len(queue) == 0 {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "*Queue:* Empty", false, false),
			nil, nil,
		))
	} else {
		queueText := fmt.Sprintf("*Queue:* (%d/%d)\n", len(queue), maxQueueSize)
		for i, item := range queue {
			// Calculate estimated wait time for this position
			estimatedWaitMinutes := i * 30
			estimatedWait := time.Duration(estimatedWaitMinutes) * time.Minute

			queueText += fmt.Sprintf("%d. <@%s> (joined %s ago)",
				i+1, item.UserID, time.Since(item.JoinedAt).Round(time.Minute))

			if i > 0 {
				queueText += fmt.Sprintf(" - estimated wait: %s", formatDuration(estimatedWait))
			} else {
				queueText += " - next in line! 🎯"
			}

			if item.Tag != "" {
				queueText += fmt.Sprintf("\n   └ waiting for %s/%s for %s", item.Environment, item.Tag, formatDuration(item.Duration))
			} else if item.Environment != "" {
				queueText += fmt.Sprintf("\n   └ waiting for %s for %s", item.Environment, formatDuration(item.Duration))
			}
			queueText += "\n"
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", queueText, false, false),
			nil, nil,
		))
	}

	// Environments and tags section (sorted, with dividers)
	for _, env := range environments {
		var availableTags, occupiedTags, maintenanceTags []*Tag
		for _, tag := range env.Tags {
			switch tag.Status {
			case "available":
				availableTags = append(availableTags, tag)
			case "occupied":
				occupiedTags = append(occupiedTags, tag)
			case "maintenance":
				maintenanceTags = append(maintenanceTags, tag)
			}
		}
		var tagFields []*slack.TextBlockObject
		// Add available tags first
		for _, tag := range availableTags {
			tagInfo := fmt.Sprintf("🟢 *%s*", tag.Name)
			tagFields = append(tagFields, slack.NewTextBlockObject("mrkdwn", tagInfo, false, false))
		}
		// Then occupied tags
		for _, tag := range occupiedTags {
			tagInfo := fmt.Sprintf("🔴 *%s*", tag.Name)
			if tag.AssignedTo != "" {
				tagInfo += fmt.Sprintf(" (<@%s>", tag.AssignedTo)
				if !tag.ExpiresAt.IsZero() {
					timeLeft := tag.ExpiresAt.Sub(time.Now())
					if timeLeft > 0 {
						tagInfo += fmt.Sprintf(", %s left", formatDuration(timeLeft))
					}
				}
				tagInfo += ")"
			}
			tagFields = append(tagFields, slack.NewTextBlockObject("mrkdwn", tagInfo, false, false))
		}
		// Then maintenance tags
		for _, tag := range maintenanceTags {
			tagInfo := fmt.Sprintf("🟡 *%s*", tag.Name)
			tagFields = append(tagFields, slack.NewTextBlockObject("mrkdwn", tagInfo, false, false))
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*%s*", env.Name), false, false),
			tagFields, nil,
		))
		blocks = append(blocks, slack.NewDividerBlock())
	}

	// Add help text
	helpText := "*Quick Commands:*\n• `@bot join [env] [tag] [duration]` - Join queue for specific tag\n• `@bot join [env] [duration]` - Join queue for any tag in env\n• `@bot leave` - Leave queue\n• `@bot position` - Check your queue position\n• `@bot assign` - Assign next user\n• `@bot list` - List all environments and tags"
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", helpText, false, false),
		nil, nil,
	))

	// Add interactive buttons
	blocks = append(blocks, qm.createInteractiveButtons()...)

	_, _, err := qm.slackClient.PostMessage(qm.channelID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		log.Printf("Error posting message: %v", err)
	}
}

// notifyUser sends a direct message to a user
func (qm *QueueManager) notifyUser(userID, message string) {
	_, _, err := qm.slackClient.PostMessage(userID, slack.MsgOptionText(message, false))
	if err != nil {
		log.Printf("Error notifying user %s: %v", userID, err)
	}
}

// notifyUserBlock sends a block message to a user (for errors/info)
func (qm *QueueManager) notifyUserBlock(userID, emoji, summary, detail, suggestion string) {
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("%s *%s*\n%s", emoji, summary, detail), false, false),
			nil, nil,
		),
	}
	if suggestion != "" {
		blocks = append(blocks, slack.NewContextBlock("suggestion", slack.NewTextBlockObject("mrkdwn", suggestion, false, false)))
	}
	_, _, err := qm.slackClient.PostMessage(userID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		log.Printf("Error sending block message to user %s: %v", userID, err)
	}
}

// createInteractiveButtons creates interactive buttons for quick actions
func (qm *QueueManager) createInteractiveButtons() []slack.Block {
	var buttons []slack.Block

	// Create a divider and quick actions section
	buttons = append(buttons, slack.NewDividerBlock())

	// Add quick actions text
	quickActionsText := "*Quick Actions:*\n"
	quickActionsText += "• `@bot join [env] [duration]` - Join queue\n"
	quickActionsText += "• `@bot leave` - Leave queue\n"
	quickActionsText += "• `@bot position` - Check your position\n"
	quickActionsText += "• `@bot assign` - Assign next user"

	buttons = append(buttons, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", quickActionsText, false, false),
		nil, nil,
	))

	return buttons
}

// ListEnvironmentsAndTags returns a formatted list of all environments and their tags
func (qm *QueueManager) ListEnvironmentsAndTags() string {
	qm.mutex.RLock()
	defer qm.mutex.RUnlock()

	result := "*Available Environments and Tags:*\n\n"
	for envName, env := range qm.environments {
		result += fmt.Sprintf("*%s:*\n", envName)
		for tagName, tag := range env.Tags {
			status := "🟢"
			if tag.Status == "occupied" {
				status = "🔴"
			} else if tag.Status == "maintenance" {
				status = "🟡"
			}
			result += fmt.Sprintf("  %s `%s` (%s)", status, tagName, tag.Status)
			if tag.AssignedTo != "" {
				result += fmt.Sprintf(" - <@%s>", tag.AssignedTo)
				if !tag.ExpiresAt.IsZero() {
					timeLeft := tag.ExpiresAt.Sub(time.Now())
					if timeLeft > 0 {
						result += fmt.Sprintf(" (expires in %s)", formatDuration(timeLeft))
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

// checkExpiredAssignments checks for and releases expired tag assignments
func (qm *QueueManager) checkExpiredAssignments() {
	qm.mutex.Lock()
	defer qm.mutex.Unlock()

	now := time.Now()
	expiredTags := []string{}

	for envName, env := range qm.environments {
		for tagName, tag := range env.Tags {
			if tag.Status == "occupied" && !tag.ExpiresAt.IsZero() && now.After(tag.ExpiresAt) {
				// Tag has expired
				userID := tag.AssignedTo
				tag.Status = "available"
				tag.AssignedTo = ""
				tag.AssignedAt = time.Time{}
				tag.ExpiresAt = time.Time{}

				expiredTags = append(expiredTags, fmt.Sprintf("%s/%s", envName, tagName))

				// Notify user that their assignment has expired
				qm.notifyUser(userID, fmt.Sprintf("⏰ Your assignment to tag *%s* in environment *%s* has expired and has been automatically released.", tagName, envName))
			}
		}
	}

	if len(expiredTags) > 0 {
		// Broadcast update to channel
		expiredMsg := fmt.Sprintf("🔄 The following tags have been automatically released due to expiration:\n• %s", strings.Join(expiredTags, "\n• "))
		qm.slackClient.PostMessage(qm.channelID, slack.MsgOptionText(expiredMsg, false))
		qm.broadcastQueueUpdate()

		// Auto-save state after expiration
		go qm.autoSave()
	}
}

// extendAssignment extends a user's assignment by 3 hours
func (qm *QueueManager) extendAssignment(userID, environment, tag string) error {
	qm.mutex.Lock()
	defer qm.mutex.Unlock()

	// Find the environment
	env, exists := qm.environments[environment]
	if !exists {
		return fmt.Errorf("environment '%s' does not exist", environment)
	}

	// Find the tag
	tagObj, tagExists := env.Tags[tag]
	if !tagExists {
		return fmt.Errorf("tag '%s' does not exist in environment '%s'", tag, environment)
	}

	// Check if user owns this tag
	if tagObj.AssignedTo != userID {
		return fmt.Errorf("you don't own tag '%s' in environment '%s'", tag, environment)
	}

	// Extend the assignment by 3 hours
	tagObj.ExpiresAt = tagObj.ExpiresAt.Add(3 * time.Hour)

	// Notify user
	newExpiryTime := tagObj.ExpiresAt.Format("15:04")
	qm.notifyUser(userID, fmt.Sprintf("⏰ Your assignment to tag *%s* in environment *%s* has been extended. New expiry time: *%s*", tag, environment, newExpiryTime))

	qm.broadcastQueueUpdate()

	// Auto-save state
	go qm.autoSave()
	return nil
}

// releaseTag releases a user's tag assignment early
func (qm *QueueManager) releaseTag(userID, environment, tag string) error {
	qm.mutex.Lock()
	defer qm.mutex.Unlock()

	// Find the environment
	env, exists := qm.environments[environment]
	if !exists {
		return fmt.Errorf("environment '%s' does not exist", environment)
	}

	// Find the tag
	tagObj, tagExists := env.Tags[tag]
	if !tagExists {
		return fmt.Errorf("tag '%s' does not exist in environment '%s'", tag, environment)
	}

	// Check if user owns this tag
	if tagObj.AssignedTo != userID {
		return fmt.Errorf("you don't own tag '%s' in environment '%s'", tag, environment)
	}

	// Release the tag
	tagObj.Status = "available"
	tagObj.AssignedTo = ""
	tagObj.AssignedAt = time.Time{}
	tagObj.ExpiresAt = time.Time{}

	// Notify user
	qm.notifyUser(userID, fmt.Sprintf("✅ You've released tag *%s* in environment *%s* early. It's now available for others.", tag, environment))

	// Notify channel about early release
	releaseMsg := fmt.Sprintf("🔄 Tag *%s* in environment *%s* has been released early by <@%s> and is now available!", tag, environment, userID)
	qm.slackClient.PostMessage(qm.channelID, slack.MsgOptionText(releaseMsg, false))

	qm.broadcastQueueUpdate()

	// Auto-save state
	go qm.autoSave()
	return nil
}

// parseDuration parses duration strings like "2h", "30m", "1h30m"
func parseDuration(durationStr string) (time.Duration, error) {
	// Try to parse as Go duration format first
	if d, err := time.ParseDuration(durationStr); err == nil {
		return d, nil
	}

	// Custom parsing for formats like "1h30m", "2h", "30m"
	var hours, minutes int

	// Check for hours
	if strings.Contains(durationStr, "h") {
		parts := strings.Split(durationStr, "h")
		if len(parts) != 2 {
			return 0, fmt.Errorf("invalid hour format")
		}
		if _, err := fmt.Sscanf(parts[0], "%d", &hours); err != nil {
			return 0, fmt.Errorf("invalid hour value")
		}
		durationStr = parts[1]
	}

	// Check for minutes
	if strings.Contains(durationStr, "m") {
		parts := strings.Split(durationStr, "m")
		if len(parts) != 2 {
			return 0, fmt.Errorf("invalid minute format")
		}
		if _, err := fmt.Sscanf(parts[0], "%d", &minutes); err != nil {
			return 0, fmt.Errorf("invalid minute value")
		}
	}

	if hours == 0 && minutes == 0 {
		return 0, fmt.Errorf("no valid duration specified")
	}

	return time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute, nil
}

// getValidDurations returns a list of valid duration options
func getValidDurations() []string {
	return []string{"30m", "1h", "1h30m", "2h", "2h30m", "3h"}
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

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Fatal("Error loading .env file")
	}

	// Initialize Slack client
	token := os.Getenv("SLACK_BOT_TOKEN")
	appToken := os.Getenv("SLACK_APP_TOKEN")
	channelID := os.Getenv("SLACK_CHANNEL_ID")
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data" // Default data directory
	}

	if token == "" || appToken == "" || channelID == "" {
		log.Fatal("Missing required environment variables: SLACK_BOT_TOKEN, SLACK_APP_TOKEN, SLACK_CHANNEL_ID")
	}

	client := slack.New(token, slack.OptionAppLevelToken(appToken))

	// Get config path from environment or use default
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "./config.json" // Default config file
	}

	queueManager := NewQueueManager(client, channelID, dataDir, configPath)

	// Load existing state
	if err := queueManager.loadState(); err != nil {
		log.Printf("Warning: Could not load existing state: %v", err)
		log.Println("Starting with fresh state...")
		if err := queueManager.initializeEnvironments(); err != nil {
			log.Fatalf("Failed to initialize environments: %v", err)
		}
	} else {
		log.Println("Successfully loaded existing state")
	}

	// Initialize socket mode
	socketClient := socketmode.New(client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start background goroutine to check for expired assignments every 5 minutes
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				queueManager.checkExpiredAssignments()
			}
		}
	}()

	// Start background goroutine to save state every 10 minutes
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				queueManager.autoSave()
			}
		}
	}()

	go func(ctx context.Context, client *slack.Client, socketClient *socketmode.Client) {
		for {
			select {
			case <-ctx.Done():
				log.Println("Shutting down socket mode listener")
				return
			case event := <-socketClient.Events:
				switch event.Type {
				case socketmode.EventTypeEventsAPI:
					eventsAPIEvent, ok := event.Data.(slackevents.EventsAPIEvent)
					if !ok {
						log.Printf("Could not type cast the event to the EventsAPIEvent: %v\n", event)
						continue
					}
					socketClient.Ack(*event.Request)
					err := handleEventMessage(eventsAPIEvent, client, queueManager)
					if err != nil {
						log.Printf("Error handling event: %v", err)
					}
				case socketmode.EventTypeInteractive:
					interactiveEvent, ok := event.Data.(slack.InteractionCallback)
					if !ok {
						log.Printf("Could not type cast the event to the InteractionCallback: %v\n", event)
						continue
					}
					socketClient.Ack(*event.Request)
					err := handleInteractiveEvent(interactiveEvent, client, queueManager)
					if err != nil {
						log.Printf("Error handling interactive event: %v", err)
					}
				}
			}
		}
	}(ctx, client, socketClient)

	// Start the socket mode client
	go func() {
		socketClient.Run()
	}()

	// Start HTTP server for health checks
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Add endpoint to manually save state
	http.HandleFunc("/save", func(w http.ResponseWriter, r *http.Request) {
		if err := queueManager.saveState(); err != nil {
			http.Error(w, fmt.Sprintf("Error saving state: %v", err), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("State saved successfully"))
	})

	log.Println("Starting server on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleEventMessage(event slackevents.EventsAPIEvent, client *slack.Client, qm *QueueManager) error {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			return handleAppMention(ev, client, qm)
		}
	}
	return nil
}

func handleInteractiveEvent(event slack.InteractionCallback, client *slack.Client, qm *QueueManager) error {
	switch event.Type {
	case slack.InteractionTypeBlockActions:
		for _, action := range event.ActionCallback.BlockActions {
			switch action.ActionID {
			case "quick_join":
				return handleQuickJoin(event, client, qm)
			case "leave_queue":
				return handleQuickLeave(event, client, qm)
			case "check_position":
				return handleCheckPosition(event, client, qm)
			case "assign_next":
				return handleAssignNext(event, client, qm)
			}
		}
	}
	return nil
}

func handleQuickJoin(event slack.InteractionCallback, client *slack.Client, qm *QueueManager) error {
	user, err := client.GetUserInfo(event.User.ID)
	if err != nil {
		return err
	}

	// For quick join, we'll use a default environment and duration
	// In a more sophisticated version, we could show a modal to select environment/duration
	err = qm.JoinQueue(event.User.ID, user.Name, "test1-au", "", 3*time.Hour)
	if err != nil {
		client.PostMessage(event.Channel.ID, slack.MsgOptionText(fmt.Sprintf("❌ %s", err.Error()), false))
	} else {
		client.PostMessage(event.Channel.ID, slack.MsgOptionText("✅ Added to queue for test1-au for 3 hours!", false))
	}
	return nil
}

func handleQuickLeave(event slack.InteractionCallback, client *slack.Client, qm *QueueManager) error {
	err := qm.LeaveQueue(event.User.ID)
	if err != nil {
		client.PostMessage(event.Channel.ID, slack.MsgOptionText(fmt.Sprintf("❌ %s", err.Error()), false))
	} else {
		client.PostMessage(event.Channel.ID, slack.MsgOptionText("✅ Removed from queue!", false))
	}
	return nil
}

func handleCheckPosition(event slack.InteractionCallback, client *slack.Client, qm *QueueManager) error {
	positionInfo := qm.GetQueuePositionInfo(event.User.ID)
	client.PostMessage(event.Channel.ID, slack.MsgOptionText(positionInfo, false))
	return nil
}

func handleAssignNext(event slack.InteractionCallback, client *slack.Client, qm *QueueManager) error {
	err := qm.AssignNextUser()
	if err != nil {
		client.PostMessage(event.Channel.ID, slack.MsgOptionText(fmt.Sprintf("❌ %s", err.Error()), false))
	} else {
		client.PostMessage(event.Channel.ID, slack.MsgOptionText("✅ Assigned next user!", false))
	}
	return nil
}

func handleAppMention(event *slackevents.AppMentionEvent, client *slack.Client, qm *QueueManager) error {
	// Extract bot user ID from the mention
	botUserID := strings.Trim(event.Text[strings.Index(event.Text, "<@"):strings.Index(event.Text, ">")+1], "<>")
	text := strings.TrimSpace(strings.Replace(event.Text, fmt.Sprintf("<@%s>", botUserID), "", -1))

	user, err := client.GetUserInfo(event.User)
	if err != nil {
		return err
	}

	parts := strings.Fields(text)
	if len(parts) == 0 {
		parts = []string{"help"}
	}

	switch parts[0] {
	case "join":
		environment := ""
		tag := ""
		duration := 3 * time.Hour // Default 3 hours

		if len(parts) > 1 {
			environment = parts[1]
		}
		if len(parts) > 2 {
			tag = parts[2]
		}
		if len(parts) > 3 {
			// Parse duration from user input (e.g., "2h", "30m", "1h30m")
			durationStr := parts[3]
			parsedDuration, err := parseDuration(durationStr)
			if err != nil {
				validOptions := strings.Join(getValidDurations(), ", ")
				client.PostMessage(event.Channel, slack.MsgOptionText(fmt.Sprintf("❌ Invalid duration format: %s. Valid options: %s", durationStr, validOptions), false))
				return nil
			}
			duration = parsedDuration
		}

		err := qm.JoinQueue(event.User, user.Name, environment, tag, duration)
		if err != nil {
			client.PostMessage(event.Channel, slack.MsgOptionText(fmt.Sprintf("❌ %s", err.Error()), false))
		} else {
			response := "✅ Added to queue!"
			if tag != "" {
				response = fmt.Sprintf("✅ Added to queue for %s/%s for %s!", environment, tag, formatDuration(duration))
			} else if environment != "" {
				response = fmt.Sprintf("✅ Added to queue for %s for %s!", environment, formatDuration(duration))
			}
			client.PostMessage(event.Channel, slack.MsgOptionText(response, false))
		}

	case "leave":
		err := qm.LeaveQueue(event.User)
		if err != nil {
			client.PostMessage(event.Channel, slack.MsgOptionText(fmt.Sprintf("❌ %s", err.Error()), false))
		} else {
			client.PostMessage(event.Channel, slack.MsgOptionText("✅ Removed from queue!", false))
		}

	case "status":
		qm.broadcastQueueUpdate()

	case "assign":
		err := qm.AssignNextUser()
		if err != nil {
			client.PostMessage(event.Channel, slack.MsgOptionText(fmt.Sprintf("❌ %s", err.Error()), false))
		} else {
			client.PostMessage(event.Channel, slack.MsgOptionText("✅ Assigned next user!", false))
		}

	case "list":
		listText := qm.ListEnvironmentsAndTags()
		client.PostMessage(event.Channel, slack.MsgOptionText(listText, false))

	case "position":
		positionInfo := qm.GetQueuePositionInfo(event.User)
		client.PostMessage(event.Channel, slack.MsgOptionText(positionInfo, false))

	case "extend":
		if len(parts) < 2 {
			client.PostMessage(event.Channel, slack.MsgOptionText("❌ Usage: @bot extend [environment] [tag]", false))
			return nil
		}

		environment := parts[1]
		tag := ""
		if len(parts) > 2 {
			tag = parts[2]
		}

		err := qm.extendAssignment(event.User, environment, tag)
		if err != nil {
			client.PostMessage(event.Channel, slack.MsgOptionText(fmt.Sprintf("❌ %s", err.Error()), false))
		} else {
			client.PostMessage(event.Channel, slack.MsgOptionText("✅ Assignment extended by 3 hours!", false))
		}

	case "release":
		if len(parts) < 2 {
			client.PostMessage(event.Channel, slack.MsgOptionText("❌ Usage: @bot release [environment] [tag]", false))
			return nil
		}

		environment := parts[1]
		tag := ""
		if len(parts) > 2 {
			tag = parts[2]
		}

		err := qm.releaseTag(event.User, environment, tag)
		if err != nil {
			client.PostMessage(event.Channel, slack.MsgOptionText(fmt.Sprintf("❌ %s", err.Error()), false))
		} else {
			client.PostMessage(event.Channel, slack.MsgOptionText("✅ Tag released early!", false))
		}

	default:
		helpText := `Available commands:
• join [environment] [tag] [duration] - Join queue for specific tag with duration (e.g., join cigna-test api 2h)
• join [environment] [duration] - Join queue for any tag in environment with duration
• leave - Leave the queue (if waiting)
• position - Check your current queue position and estimated wait time
• release [environment] [tag] - Release your tag early (if assigned)
• status - Show current queue and environment status
• assign - Assign next user in queue to available tag
• list - List all environments and available tags
• extend [environment] [tag] - Extend your assignment by 3 hours

Duration options (30-minute intervals):
• 30m = 30 minutes
• 1h = 1 hour
• 1h30m = 1 hour 30 minutes
• 2h = 2 hours
• 2h30m = 2 hours 30 minutes
• 3h = 3 hours
• Default: 3 hours (if no duration specified)

Examples:
• @bot join cigna-test api 2h
• @bot join test1-au 1h30m
• @bot join test1-au api 30m
• @bot join test1-au api (defaults to 3h)
• @bot position - Check your queue position
• @bot release test1-au api
• @bot extend test1-au api
• @bot list`
		client.PostMessage(event.Channel, slack.MsgOptionText(helpText, false))
	}

	return nil
}
