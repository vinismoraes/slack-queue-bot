package utils

import (
	"fmt"
	"strings"
	"time"
)

// ParseDuration parses duration strings like "2h", "30m", "1h30m"
func ParseDuration(durationStr string) (time.Duration, error) {
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

// GetValidDurations returns a list of valid duration options
func GetValidDurations() []string {
	return []string{"30m", "1h", "1h30m", "2h", "2h30m", "3h"}
}

// FormatDuration formats a duration in a human-readable way
func FormatDuration(d time.Duration) string {
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

// ValidateDuration validates if a duration is within acceptable bounds
func ValidateDuration(d time.Duration, min, max time.Duration) error {
	if d < min {
		return fmt.Errorf("duration %s is less than minimum %s", FormatDuration(d), FormatDuration(min))
	}
	if d > max {
		return fmt.Errorf("duration %s is greater than maximum %s", FormatDuration(d), FormatDuration(max))
	}

	// Check if duration is in 30-minute intervals
	if d%(30*time.Minute) != 0 {
		validOptions := strings.Join(GetValidDurations(), ", ")
		return fmt.Errorf("duration must be in 30-minute intervals. Valid options: %s", validOptions)
	}

	return nil
}
