package services

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"slack-queue-bot/internal/interfaces"
)

// BackgroundExpirationService handles automatic tag expiration in the background
// This service runs independently and uses proper locking to avoid race conditions
type BackgroundExpirationService struct {
	db            interfaces.DatabaseRepository
	notification  interfaces.NotificationService
	queueService  interfaces.QueueService
	ticker        *time.Ticker
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.RWMutex
	running       bool
	checkInterval time.Duration
	lastLogTime   time.Time // Track when we last logged "no expired tags"
}

// NewBackgroundExpirationService creates a new background expiration service
func NewBackgroundExpirationService(
	db interfaces.DatabaseRepository,
	notification interfaces.NotificationService,
	queueService interfaces.QueueService,
) *BackgroundExpirationService {
	return &BackgroundExpirationService{
		db:            db,
		notification:  notification,
		queueService:  queueService,
		checkInterval: 30 * time.Second, // Check every 30 seconds for better responsiveness
	}
}

// Start begins the background expiration checking
func (bes *BackgroundExpirationService) Start() error {
	bes.mu.Lock()
	defer bes.mu.Unlock()

	if bes.running {
		return nil // Already running
	}

	log.Printf("Starting background expiration service (check interval: %v)", bes.checkInterval)

	bes.ctx, bes.cancel = context.WithCancel(context.Background())
	bes.ticker = time.NewTicker(bes.checkInterval)
	bes.running = true

	// Start the background goroutine
	go bes.expirationLoop()

	// Do an initial cleanup on startup
	go func() {
		time.Sleep(5 * time.Second) // Wait for services to fully initialize
		bes.performSafeExpiration()
	}()

	return nil
}

// Stop stops the background expiration service
func (bes *BackgroundExpirationService) Stop() error {
	bes.mu.Lock()
	defer bes.mu.Unlock()

	if !bes.running {
		return nil // Already stopped
	}

	log.Println("Stopping background expiration service")

	bes.cancel()
	bes.ticker.Stop()
	bes.running = false

	return nil
}

// IsRunning returns whether the service is currently running
func (bes *BackgroundExpirationService) IsRunning() bool {
	bes.mu.RLock()
	defer bes.mu.RUnlock()
	return bes.running
}

// expirationLoop runs the periodic expiration check
func (bes *BackgroundExpirationService) expirationLoop() {
	log.Println("Background expiration loop started")

	for {
		select {
		case <-bes.ctx.Done():
			log.Println("Background expiration loop stopped")
			return
		case <-bes.ticker.C:
			bes.performSafeExpiration()
		}
	}
}

// performSafeExpiration safely checks and releases expired tags
// This method is designed to avoid race conditions with assignment operations
func (bes *BackgroundExpirationService) performSafeExpiration() {
	// Get expired tags using the database view (read-only operation)
	expiredTags, err := bes.db.GetExpiredTagsFromView()
	if err != nil {
		log.Printf("Error getting expired tags: %v", err)
		return
	}

	if len(expiredTags) == 0 {
		// Only log "no expired tags" every 5 minutes to reduce noise
		if time.Since(bes.lastLogTime) > 5*time.Minute {
			log.Printf("Background expiration check: No expired tags found")
			bes.lastLogTime = time.Now()
		}
		return
	}

	log.Printf("Background expiration: Found %d expired tags to process", len(expiredTags))

	var releasedTags []string
	var releasedUsers []string

	// Process each expired tag individually with error handling
	for _, tag := range expiredTags {
		assignedTo := ""
		if tag.AssignedTo != "" {
			assignedTo = tag.AssignedTo
		}

		if assignedTo == "" {
			log.Printf("Skipping expired tag %s/%s - no assigned user", tag.Environment, tag.Name)
			continue
		}

		// Calculate how long it's been expired
		timeExpired := time.Now().UTC().Sub(tag.ExpiresAt)
		log.Printf("Processing expired tag: %s/%s (user: %s, expired %v ago)",
			tag.Environment, tag.Name, assignedTo, timeExpired.Round(time.Minute))

		// Release the tag using the safe database operation
		// This operation is atomic and won't conflict with assignments
		err := bes.db.UpdateTagStatus(tag.Environment, tag.Name, "available", nil, nil)
		if err != nil {
			log.Printf("Error releasing expired tag %s/%s: %v", tag.Environment, tag.Name, err)
			continue
		}

		log.Printf("Released expired tag: %s/%s (was assigned to %s)", tag.Environment, tag.Name, assignedTo)

		// Track for notifications
		releasedTags = append(releasedTags, fmt.Sprintf("%s/%s", tag.Environment, tag.Name))
		releasedUsers = append(releasedUsers, fmt.Sprintf("<@%s>", assignedTo))

		// Individual notifications disabled for background expiration to avoid channel spam
	}

	// Send summary notification to channel if any tags were released
	if len(releasedTags) > 0 {
		log.Printf("Successfully released %d expired tags: %v", len(releasedTags), releasedTags)

		// Trigger queue processing to assign newly available tags
		if bes.queueService != nil {
			go func() {
				time.Sleep(1 * time.Second) // Small delay to ensure database updates are committed
				if err := bes.queueService.ProcessQueue(); err != nil {
					log.Printf("Error processing queue after expiration: %v", err)
				}
			}()
		}

		// Silent operation - no channel notifications for automatic expiration
		log.Printf("Background expiration complete - %d tags released silently", len(releasedTags))
	}
}

// sendExpirationSummary sends a consolidated notification about expired tags
func (bes *BackgroundExpirationService) sendExpirationSummary(releasedTags, releasedUsers []string) {
	// Send a single, non-spammy notification to the channel
	if len(releasedTags) == 1 {
		message := fmt.Sprintf("⏰ **Auto-expired:** %s (was assigned to %s)",
			releasedTags[0], releasedUsers[0])
		log.Printf("Sending expiration notification: %s", message)
		// Keep as log to avoid channel spam
	} else if len(releasedTags) > 1 {
		// Group by user for cleaner display
		userTagMap := make(map[string][]string)
		for i, tag := range releasedTags {
			if i < len(releasedUsers) {
				user := releasedUsers[i]
				userTagMap[user] = append(userTagMap[user], tag)
			}
		}

		message := fmt.Sprintf("⏰ **Auto-expired %d assignments**", len(releasedTags))
		log.Printf("Sending batch expiration notification: %s", message)
		// Keep as log to avoid channel spam
	}
}

// SetCheckInterval allows changing the expiration check frequency
func (bes *BackgroundExpirationService) SetCheckInterval(interval time.Duration) {
	bes.mu.Lock()
	defer bes.mu.Unlock()

	oldInterval := bes.checkInterval
	bes.checkInterval = interval

	if bes.running && bes.ticker != nil {
		bes.ticker.Stop()
		bes.ticker = time.NewTicker(interval)
		log.Printf("Updated expiration check interval from %v to %v", oldInterval, interval)
	} else {
		log.Printf("Expiration check interval set to %v (will take effect when started)", interval)
	}
}

// ForceCheck triggers an immediate expiration check (useful for testing)
func (bes *BackgroundExpirationService) ForceCheck() {
	log.Println("Force checking for expired tags...")
	go bes.performSafeExpiration()
}
