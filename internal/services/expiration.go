package services

import (
	"fmt"
	"log"
	"slack-queue-bot/internal/database"
	"slack-queue-bot/internal/interfaces"
	"slack-queue-bot/internal/models"
)

// ExpirationService handles real-time expiration events from the database
type ExpirationService struct {
	db               interfaces.DatabaseRepository
	notification     interfaces.NotificationService
	queueService     interfaces.QueueService
	notificationChan chan interface{}
}

// NewExpirationService creates a new expiration service
func NewExpirationService(
	db interfaces.DatabaseRepository,
	notification interfaces.NotificationService,
	queueService interfaces.QueueService,
) *ExpirationService {
	return &ExpirationService{
		db:               db,
		notification:     notification,
		queueService:     queueService,
		notificationChan: make(chan interface{}, 100), // Buffered channel
	}
}

// StartExpirationListener starts listening for database expiration events
func (es *ExpirationService) StartExpirationListener(dbPath string) error {
	log.Println("Starting expiration notification service...")

	// Start the database listener
	if err := es.db.StartExpirationListener(es.notificationChan, dbPath); err != nil {
		return fmt.Errorf("failed to start database expiration listener: %w", err)
	}

	// Start the notification processor
	go es.processExpirationNotifications()

	return nil
}

// processExpirationNotifications processes expiration events from the database
func (es *ExpirationService) processExpirationNotifications() {
	log.Println("Starting expiration notification processor...")

	for notificationInterface := range es.notificationChan {
		// Type assert to ExpirationNotification
		notification, ok := notificationInterface.(database.ExpirationNotification)
		if !ok {
			log.Printf("Error: received invalid notification type: %T", notificationInterface)
			continue
		}

		log.Printf("Processing expiration notification for %s/%s (user: %s)",
			notification.Environment, notification.TagName, notification.AssignedTo)

		// Create a Tag model for the notification
		tag := &models.Tag{
			Name:        notification.TagName,
			Status:      "available", // It's now available since it expired
			AssignedTo:  "",          // No longer assigned
			Environment: notification.Environment,
			ExpiresAt:   notification.ExpiredAt,
		}

		// Send expiration notification to the user
		if err := es.notification.NotifyExpiration(notification.AssignedTo, tag, notification.Environment); err != nil {
			log.Printf("Error sending expiration notification to user %s: %v", notification.AssignedTo, err)
		}

		// Broadcast queue update to show the newly available tag
		if err := es.notification.BroadcastQueueUpdate(); err != nil {
			log.Printf("Error broadcasting queue update after expiration: %v", err)
		}

		// Process the queue to potentially assign the newly available tag
		if es.queueService != nil {
			go func() {
				if err := es.queueService.ProcessQueue(); err != nil {
					log.Printf("Error processing queue after expiration: %v", err)
				}
			}()
		}

		log.Printf("Successfully processed expiration for %s/%s", notification.Environment, notification.TagName)
	}
}

// GetNotificationChannel returns the notification channel for manual triggering
func (es *ExpirationService) GetNotificationChannel() chan<- interface{} {
	return es.notificationChan
}

// Stop stops the expiration service
func (es *ExpirationService) Stop() {
	close(es.notificationChan)
}
