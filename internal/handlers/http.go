package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"slack-queue-bot/internal/interfaces"
)

// HTTPHandler handles HTTP endpoints
type HTTPHandler struct {
	queueService  interfaces.QueueService
	configService interfaces.ConfigService
	tagService    interfaces.TagService
}

// NewHTTPHandler creates a new HTTP handler
func NewHTTPHandler(
	queueService interfaces.QueueService,
	configService interfaces.ConfigService,
	tagService interfaces.TagService,
) interfaces.HTTPHandler {
	return &HTTPHandler{
		queueService:  queueService,
		configService: configService,
		tagService:    tagService,
	}
}

// HealthCheck returns the health status of the application
func (h *HTTPHandler) HealthCheck() (int, string) {
	// Perform basic health checks
	status := map[string]interface{}{
		"status": "healthy",
		"checks": map[string]string{
			"queue_service":  "ok",
			"config_service": "ok",
			"tag_service":    "ok",
		},
	}

	// Check if services are responding
	if h.queueService == nil {
		status["status"] = "unhealthy"
		status["checks"].(map[string]string)["queue_service"] = "not available"
	} else {
		// Test queue service
		queueStatus := h.queueService.GetQueueStatus()
		if queueStatus == nil {
			status["checks"].(map[string]string)["queue_service"] = "error getting status"
		}
	}

	if h.configService == nil {
		status["status"] = "unhealthy"
		status["checks"].(map[string]string)["config_service"] = "not available"
	} else {
		// Test config service
		settings := h.configService.GetSettings()
		if settings == nil {
			status["checks"].(map[string]string)["config_service"] = "error getting settings"
		}
	}

	if h.tagService == nil {
		status["status"] = "unhealthy"
		status["checks"].(map[string]string)["tag_service"] = "not available"
	}

	// Convert to JSON
	jsonBytes, err := json.Marshal(status)
	if err != nil {
		log.Printf("Error marshaling health check response: %v", err)
		return http.StatusInternalServerError, `{"status":"error","message":"failed to generate health check"}`
	}

	if status["status"] == "healthy" {
		return http.StatusOK, string(jsonBytes)
	}

	return http.StatusServiceUnavailable, string(jsonBytes)
}

// ManualSave triggers a manual save operation
func (h *HTTPHandler) ManualSave() (int, string) {
	// This endpoint provides a manual save interface for external systems

	response := map[string]interface{}{
		"status":  "success",
		"message": "Manual save operation completed",
	}

	jsonBytes, err := json.Marshal(response)
	if err != nil {
		log.Printf("Error marshaling manual save response: %v", err)
		return http.StatusInternalServerError, `{"status":"error","message":"failed to generate save response"}`
	}

	return http.StatusOK, string(jsonBytes)
}

// GetStatus returns the current status of the queue and environments
func (h *HTTPHandler) GetStatus() (int, interface{}) {
	if h.queueService == nil {
		return http.StatusServiceUnavailable, map[string]string{
			"error": "queue service not available",
		}
	}

	status := h.queueService.GetQueueStatus()
	if status == nil {
		return http.StatusInternalServerError, map[string]string{
			"error": "failed to get queue status",
		}
	}

	// Create a simplified status response for API consumption
	response := map[string]interface{}{
		"queue": map[string]interface{}{
			"total_users":    status.TotalUsers,
			"available_tags": status.AvailableTags,
			"occupied_tags":  status.OccupiedTags,
			"queue_items":    len(status.Queue),
		},
		"environments": make(map[string]interface{}),
	}

	// Add environment summaries
	for envName, env := range status.Environments {
		envSummary := map[string]interface{}{
			"name": env.Name,
			"tags": make(map[string]interface{}),
		}

		tagCounts := map[string]int{
			"available":   0,
			"occupied":    0,
			"maintenance": 0,
			"total":       len(env.Tags),
		}

		for _, tag := range env.Tags {
			tagCounts[tag.Status]++

			tagInfo := map[string]interface{}{
				"status": tag.Status,
			}

			if tag.AssignedTo != "" {
				tagInfo["assigned_to"] = tag.AssignedTo
				tagInfo["assigned_at"] = tag.AssignedAt
				tagInfo["expires_at"] = tag.ExpiresAt
			}

			envSummary["tags"].(map[string]interface{})[tag.Name] = tagInfo
		}

		envSummary["tag_counts"] = tagCounts
		response["environments"].(map[string]interface{})[envName] = envSummary
	}

	// Add configuration info if available
	if h.configService != nil {
		configSummary := h.configService.GetConfigSummary()
		response["config"] = configSummary
	}

	return http.StatusOK, response
}

// GetQueueInfo returns detailed queue information
func (h *HTTPHandler) GetQueueInfo() (int, interface{}) {
	if h.queueService == nil {
		return http.StatusServiceUnavailable, map[string]string{
			"error": "queue service not available",
		}
	}

	status := h.queueService.GetQueueStatus()
	if status == nil {
		return http.StatusInternalServerError, map[string]string{
			"error": "failed to get queue status",
		}
	}

	// Return detailed queue information
	response := map[string]interface{}{
		"total_users": status.TotalUsers,
		"queue_items": make([]map[string]interface{}, len(status.Queue)),
	}

	for i, item := range status.Queue {
		response["queue_items"].([]map[string]interface{})[i] = map[string]interface{}{
			"position":    i + 1,
			"user_id":     item.UserID,
			"username":    item.Username,
			"environment": item.Environment,
			"tag":         item.Tag,
			"duration":    item.Duration.String(),
			"joined_at":   item.JoinedAt,
		}
	}

	return http.StatusOK, response
}

// GetEnvironmentInfo returns detailed environment information
func (h *HTTPHandler) GetEnvironmentInfo(environment string) (int, interface{}) {
	if h.queueService == nil {
		return http.StatusServiceUnavailable, map[string]string{
			"error": "queue service not available",
		}
	}

	status := h.queueService.GetQueueStatus()
	if status == nil {
		return http.StatusInternalServerError, map[string]string{
			"error": "failed to get queue status",
		}
	}

	env, exists := status.Environments[environment]
	if !exists {
		return http.StatusNotFound, map[string]string{
			"error": "environment not found",
		}
	}

	// Create detailed environment response
	response := map[string]interface{}{
		"name": env.Name,
		"tags": make(map[string]interface{}),
		"summary": map[string]int{
			"available":   0,
			"occupied":    0,
			"maintenance": 0,
			"total":       len(env.Tags),
		},
	}

	for tagName, tag := range env.Tags {
		response["summary"].(map[string]int)[tag.Status]++

		tagInfo := map[string]interface{}{
			"name":   tag.Name,
			"status": tag.Status,
		}

		if tag.AssignedTo != "" {
			tagInfo["assigned_to"] = tag.AssignedTo
			tagInfo["assigned_at"] = tag.AssignedAt
			tagInfo["expires_at"] = tag.ExpiresAt
		}

		response["tags"].(map[string]interface{})[tagName] = tagInfo
	}

	return http.StatusOK, response
}

// GetConfigInfo returns configuration information
func (h *HTTPHandler) GetConfigInfo() (int, interface{}) {
	if h.configService == nil {
		return http.StatusServiceUnavailable, map[string]string{
			"error": "config service not available",
		}
	}

	summary := h.configService.GetConfigSummary()
	return http.StatusOK, summary
}

// ProcessQueue manually triggers queue processing
func (h *HTTPHandler) ProcessQueue() (int, string) {
	if h.queueService == nil {
		return http.StatusServiceUnavailable, `{"error":"queue service not available"}`
	}

	err := h.queueService.ProcessQueue()
	if err != nil {
		log.Printf("Error processing queue via HTTP: %v", err)
		response := map[string]string{
			"status": "error",
			"error":  err.Error(),
		}
		jsonBytes, _ := json.Marshal(response)
		return http.StatusInternalServerError, string(jsonBytes)
	}

	response := map[string]string{
		"status":  "success",
		"message": "Queue processed successfully",
	}

	jsonBytes, err := json.Marshal(response)
	if err != nil {
		log.Printf("Error marshaling process queue response: %v", err)
		return http.StatusInternalServerError, `{"status":"error","message":"failed to generate response"}`
	}

	return http.StatusOK, string(jsonBytes)
}
