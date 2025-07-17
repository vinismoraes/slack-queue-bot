package services

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"slack-queue-bot/internal/models"
)

// InMemoryStorage implements storage functionality for the queue bot
type InMemoryStorage struct {
	queue        []models.QueueItem
	environments map[string]*models.Environment
	mutex        sync.RWMutex
	dataDir      string
	lastSaved    time.Time
}

// NewInMemoryStorage creates a new in-memory storage instance
func NewInMemoryStorage(dataDir string) *InMemoryStorage {
	return &InMemoryStorage{
		queue:        make([]models.QueueItem, 0),
		environments: make(map[string]*models.Environment),
		dataDir:      dataDir,
		lastSaved:    time.Now(),
	}
}

// Initialize sets up the storage with configuration data
func (s *InMemoryStorage) Initialize(config *models.Config) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Initialize environments from configuration
	for _, envConfig := range config.Environments {
		env := &models.Environment{
			Name: envConfig.Name,
			Tags: make(map[string]*models.Tag),
		}

		for _, tagName := range envConfig.Tags {
			env.Tags[tagName] = &models.Tag{
				Name:        tagName,
				Status:      "available",
				Environment: envConfig.Name,
			}
		}

		s.environments[envConfig.Name] = env
	}

	log.Printf("Initialized %d environments from configuration", len(config.Environments))
	return nil
}

// SaveState saves the current state to JSON files
func (s *InMemoryStorage) SaveState() error {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	// Create data directory if it doesn't exist
	if err := os.MkdirAll(s.dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %v", err)
	}

	// Prepare data for saving
	data := models.PersistenceData{
		Queue:        make([]models.QueueItem, len(s.queue)),
		Environments: make(map[string]*models.Environment),
		LastSaved:    time.Now(),
	}

	// Copy queue
	copy(data.Queue, s.queue)

	// Copy environments
	for k, v := range s.environments {
		data.Environments[k] = &models.Environment{
			Name: v.Name,
			Tags: make(map[string]*models.Tag),
		}
		for tagK, tagV := range v.Tags {
			data.Environments[k].Tags[tagK] = &models.Tag{
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
	filePath := fmt.Sprintf("%s/queue_state.json", s.dataDir)
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

	s.lastSaved = time.Now()
	log.Printf("State saved to %s", filePath)
	return nil
}

// LoadState loads the state from JSON files
func (s *InMemoryStorage) LoadState() error {
	filePath := fmt.Sprintf("%s/queue_state.json", s.dataDir)

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

	var data models.PersistenceData
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&data); err != nil {
		return fmt.Errorf("failed to decode state: %v", err)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Load queue
	s.queue = make([]models.QueueItem, len(data.Queue))
	copy(s.queue, data.Queue)

	// Load environments
	s.environments = make(map[string]*models.Environment)
	for k, v := range data.Environments {
		s.environments[k] = &models.Environment{
			Name: v.Name,
			Tags: make(map[string]*models.Tag),
		}
		for tagK, tagV := range v.Tags {
			s.environments[k].Tags[tagK] = &models.Tag{
				Name:        tagV.Name,
				Status:      tagV.Status,
				AssignedTo:  tagV.AssignedTo,
				AssignedAt:  tagV.AssignedAt,
				ExpiresAt:   tagV.ExpiresAt,
				Environment: tagV.Environment,
			}
		}
	}

	s.lastSaved = data.LastSaved
	log.Printf("State loaded from %s (last saved: %s)", filePath, s.lastSaved.Format(time.RFC3339))
	return nil
}

// GetQueue returns a copy of the current queue
func (s *InMemoryStorage) GetQueue() []models.QueueItem {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	queueCopy := make([]models.QueueItem, len(s.queue))
	copy(queueCopy, s.queue)
	return queueCopy
}

// GetEnvironments returns a copy of all environments
func (s *InMemoryStorage) GetEnvironments() map[string]*models.Environment {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	envCopy := make(map[string]*models.Environment)
	for k, v := range s.environments {
		envCopy[k] = &models.Environment{
			Name: v.Name,
			Tags: make(map[string]*models.Tag),
		}
		for tagK, tagV := range v.Tags {
			envCopy[k].Tags[tagK] = &models.Tag{
				Name:        tagV.Name,
				Status:      tagV.Status,
				AssignedTo:  tagV.AssignedTo,
				AssignedAt:  tagV.AssignedAt,
				ExpiresAt:   tagV.ExpiresAt,
				Environment: tagV.Environment,
			}
		}
	}
	return envCopy
}

// GetEnvironment returns a specific environment
func (s *InMemoryStorage) GetEnvironment(name string) (*models.Environment, bool) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	env, exists := s.environments[name]
	if !exists {
		return nil, false
	}

	// Return a copy
	envCopy := &models.Environment{
		Name: env.Name,
		Tags: make(map[string]*models.Tag),
	}
	for tagK, tagV := range env.Tags {
		envCopy.Tags[tagK] = &models.Tag{
			Name:        tagV.Name,
			Status:      tagV.Status,
			AssignedTo:  tagV.AssignedTo,
			AssignedAt:  tagV.AssignedAt,
			ExpiresAt:   tagV.ExpiresAt,
			Environment: tagV.Environment,
		}
	}
	return envCopy, true
}

// GetTag returns a specific tag from an environment
func (s *InMemoryStorage) GetTag(environment, tagName string) (*models.Tag, bool) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	env, exists := s.environments[environment]
	if !exists {
		return nil, false
	}

	tag, exists := env.Tags[tagName]
	if !exists {
		return nil, false
	}

	// Return a copy
	return &models.Tag{
		Name:        tag.Name,
		Status:      tag.Status,
		AssignedTo:  tag.AssignedTo,
		AssignedAt:  tag.AssignedAt,
		ExpiresAt:   tag.ExpiresAt,
		Environment: tag.Environment,
	}, true
}

// AddToQueue adds a queue item
func (s *InMemoryStorage) AddToQueue(item models.QueueItem) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.queue = append(s.queue, item)
}

// RemoveFromQueue removes a queue item by user ID
func (s *InMemoryStorage) RemoveFromQueue(userID string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for i, item := range s.queue {
		if item.UserID == userID {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return true
		}
	}
	return false
}

// GetQueueItem returns a queue item for a specific user
func (s *InMemoryStorage) GetQueueItem(userID string) (*models.QueueItem, int) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	for i, item := range s.queue {
		if item.UserID == userID {
			return &item, i
		}
	}
	return nil, -1
}

// GetFirstQueueItem returns the first item in queue and removes it
func (s *InMemoryStorage) GetFirstQueueItem() (*models.QueueItem, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if len(s.queue) == 0 {
		return nil, false
	}

	item := s.queue[0]
	s.queue = s.queue[1:]
	return &item, true
}

// UpdateTag updates a tag's properties
func (s *InMemoryStorage) UpdateTag(environment, tagName string, updater func(*models.Tag)) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	env, exists := s.environments[environment]
	if !exists {
		return fmt.Errorf("environment '%s' does not exist", environment)
	}

	tag, exists := env.Tags[tagName]
	if !exists {
		return fmt.Errorf("tag '%s' does not exist in environment '%s'", tagName, environment)
	}

	updater(tag)
	return nil
}

// GetAssignedTag finds a tag assigned to a specific user
func (s *InMemoryStorage) GetAssignedTag(userID string) (*models.Tag, bool) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	for _, env := range s.environments {
		for _, tag := range env.Tags {
			if tag.AssignedTo == userID && tag.Status == "occupied" {
				// Return a copy
				return &models.Tag{
					Name:        tag.Name,
					Status:      tag.Status,
					AssignedTo:  tag.AssignedTo,
					AssignedAt:  tag.AssignedAt,
					ExpiresAt:   tag.ExpiresAt,
					Environment: tag.Environment,
				}, true
			}
		}
	}
	return nil, false
}

// GetAvailableTags returns all available tags
func (s *InMemoryStorage) GetAvailableTags() []*models.Tag {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	var available []*models.Tag
	for _, env := range s.environments {
		for _, tag := range env.Tags {
			if tag.Status == "available" {
				available = append(available, &models.Tag{
					Name:        tag.Name,
					Status:      tag.Status,
					AssignedTo:  tag.AssignedTo,
					AssignedAt:  tag.AssignedAt,
					ExpiresAt:   tag.ExpiresAt,
					Environment: tag.Environment,
				})
			}
		}
	}
	return available
}

// GetExpiredTags returns all tags that have expired
func (s *InMemoryStorage) GetExpiredTags() []*models.Tag {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	var expired []*models.Tag
	now := time.Now()

	for _, env := range s.environments {
		for _, tag := range env.Tags {
			if tag.Status == "occupied" && !tag.ExpiresAt.IsZero() && now.After(tag.ExpiresAt) {
				expired = append(expired, &models.Tag{
					Name:        tag.Name,
					Status:      tag.Status,
					AssignedTo:  tag.AssignedTo,
					AssignedAt:  tag.AssignedAt,
					ExpiresAt:   tag.ExpiresAt,
					Environment: tag.Environment,
				})
			}
		}
	}
	return expired
}

// IsQueueEmpty returns true if the queue is empty
func (s *InMemoryStorage) IsQueueEmpty() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return len(s.queue) == 0
}

// GetQueueSize returns the current queue size
func (s *InMemoryStorage) GetQueueSize() int {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return len(s.queue)
}

// GetLastSaved returns when the state was last saved
func (s *InMemoryStorage) GetLastSaved() time.Time {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.lastSaved
}
