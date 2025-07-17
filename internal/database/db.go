package database

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	conn *sql.DB
}

// NewDB creates a new database connection
func NewDB(dataDir string) (*DB, error) {
	dbPath := fmt.Sprintf("%s/queue.db", dataDir)

	conn, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test connection
	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	db := &DB{conn: conn}

	// Initialize schema
	if err := db.initSchema(); err != nil {
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return db, nil
}

// Close closes the database connection
func (db *DB) Close() error {
	return db.conn.Close()
}

// initSchema creates the database tables
func (db *DB) initSchema() error {
	schema := `
	-- Environments table
	CREATE TABLE IF NOT EXISTS environments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	-- Tags table
	CREATE TABLE IF NOT EXISTS tags (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		environment_id INTEGER NOT NULL,
		status TEXT NOT NULL DEFAULT 'available', -- 'available', 'occupied', 'maintenance'
		assigned_to TEXT,
		assigned_at DATETIME,
		expires_at DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (environment_id) REFERENCES environments (id) ON DELETE CASCADE,
		UNIQUE(name, environment_id)
	);

	-- Configuration settings table
	CREATE TABLE IF NOT EXISTS config_settings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		setting_key TEXT NOT NULL UNIQUE,
		setting_value TEXT NOT NULL,
		setting_type TEXT NOT NULL DEFAULT 'string', -- 'string', 'integer', 'duration'
		description TEXT,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	-- Queue table
	CREATE TABLE IF NOT EXISTS queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id TEXT NOT NULL,
		username TEXT NOT NULL,
		environment TEXT NOT NULL,
		tag TEXT NOT NULL,
		duration_minutes INTEGER NOT NULL,
		joined_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		position INTEGER NOT NULL
	);

	-- TTL expiration events table for automatic expiration handling
	CREATE TABLE IF NOT EXISTS expiration_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tag_id INTEGER NOT NULL,
		environment TEXT NOT NULL,
		tag_name TEXT NOT NULL,
		assigned_to TEXT NOT NULL,
		expires_at DATETIME NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		processed BOOLEAN DEFAULT FALSE,
		FOREIGN KEY (tag_id) REFERENCES tags (id) ON DELETE CASCADE
	);

	-- Auto-expiration tracking table for database-driven expiration
	CREATE TABLE IF NOT EXISTS auto_expiration_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tag_id INTEGER NOT NULL,
		environment TEXT NOT NULL,
		tag_name TEXT NOT NULL,
		assigned_to TEXT NOT NULL,
		expired_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		processed_by TEXT DEFAULT 'database_trigger'
	);

	-- View for easy identification of expired tags
	CREATE VIEW IF NOT EXISTS expired_tags_view AS
	SELECT
		t.id,
		t.name,
		e.name as environment,
		t.assigned_to,
		t.expires_at,
		(julianday('now') - julianday(t.expires_at)) * 24 * 60 as minutes_overdue
	FROM tags t
	JOIN environments e ON t.environment_id = e.id
	WHERE t.status = 'occupied'
		AND t.expires_at IS NOT NULL
		AND t.expires_at < datetime('now');

	-- Indexes for better performance
	CREATE INDEX IF NOT EXISTS idx_tags_environment_status ON tags (environment_id, status);
	CREATE INDEX IF NOT EXISTS idx_tags_assigned_to ON tags (assigned_to);
	CREATE INDEX IF NOT EXISTS idx_tags_expires_at ON tags (expires_at);
	CREATE INDEX IF NOT EXISTS idx_queue_user_id ON queue (user_id);
	CREATE INDEX IF NOT EXISTS idx_queue_position ON queue (position);
	CREATE INDEX IF NOT EXISTS idx_expiration_events_expires_at ON expiration_events (expires_at);
	CREATE INDEX IF NOT EXISTS idx_expiration_events_processed ON expiration_events (processed);
	CREATE INDEX IF NOT EXISTS idx_config_settings_key ON config_settings (setting_key);

	-- TTL Trigger: Automatically create expiration events when tags are assigned
	CREATE TRIGGER IF NOT EXISTS trigger_tag_assignment_ttl
		AFTER UPDATE OF status, assigned_to, expires_at ON tags
		WHEN NEW.status = 'occupied' AND NEW.assigned_to IS NOT NULL AND NEW.expires_at IS NOT NULL
	BEGIN
		-- Remove any existing expiration event for this tag
		DELETE FROM expiration_events WHERE tag_id = NEW.id AND processed = FALSE;

		-- Create new expiration event
		INSERT INTO expiration_events (tag_id, environment, tag_name, assigned_to, expires_at)
		SELECT NEW.id, e.name, NEW.name, NEW.assigned_to, NEW.expires_at
		FROM environments e WHERE e.id = NEW.environment_id;
	END;

	-- TTL Trigger: Clean up expiration events when tags are released
	CREATE TRIGGER IF NOT EXISTS trigger_tag_release_ttl
		AFTER UPDATE OF status, assigned_to ON tags
		WHEN NEW.status = 'available' OR NEW.assigned_to IS NULL
	BEGIN
		-- Mark expiration events as processed when tag is manually released
		UPDATE expiration_events
		SET processed = TRUE
		WHERE tag_id = NEW.id AND processed = FALSE;
	END;

	-- TTL Trigger: Clean up expiration events when tags are deleted
	CREATE TRIGGER IF NOT EXISTS trigger_tag_delete_ttl
		AFTER DELETE ON tags
	BEGIN
		-- Clean up any expiration events for deleted tags
		DELETE FROM expiration_events WHERE tag_id = OLD.id;
	END;

		-- AUTO-EXPIRATION TRIGGER: Check and release expired tags on any UPDATE operation
	CREATE TRIGGER IF NOT EXISTS trigger_auto_expire_on_update
		BEFORE UPDATE ON tags
	BEGIN
		-- First, handle any expired tags in the system before processing the current update
		UPDATE tags
		SET status = 'available', assigned_to = NULL, assigned_at = NULL, expires_at = NULL
		WHERE status = 'occupied'
			AND expires_at IS NOT NULL
			AND expires_at < datetime('now')
			AND id != NEW.id; -- Don't interfere with the current update

		-- Log auto-expirations for expired tags (excluding the current update)
		INSERT INTO auto_expiration_log (tag_id, environment, tag_name, assigned_to)
		SELECT t.id, e.name, t.name, t.assigned_to
		FROM tags t
		JOIN environments e ON t.environment_id = e.id
		WHERE t.status = 'occupied'
			AND t.expires_at IS NOT NULL
			AND t.expires_at < datetime('now')
			AND t.id != NEW.id;

		-- Mark expiration events as processed for expired tags
		UPDATE expiration_events
		SET processed = TRUE
		WHERE expires_at < datetime('now') AND processed = FALSE;
	END;

	-- AUTO-EXPIRATION TRIGGER: Check expired tags on queue operations
	CREATE TRIGGER IF NOT EXISTS trigger_auto_expire_on_queue_change
		BEFORE INSERT ON queue
	BEGIN
		-- Check and release any expired tags before adding to queue
		UPDATE tags
		SET status = 'available', assigned_to = NULL, assigned_at = NULL, expires_at = NULL
		WHERE status = 'occupied'
			AND expires_at IS NOT NULL
			AND expires_at < datetime('now');

		-- Log auto-expirations
		INSERT INTO auto_expiration_log (tag_id, environment, tag_name, assigned_to)
		SELECT t.id, e.name, t.name, t.assigned_to
		FROM tags t
		JOIN environments e ON t.environment_id = e.id
		WHERE t.status = 'occupied'
			AND t.expires_at IS NOT NULL
			AND t.expires_at < datetime('now');

		-- Mark expiration events as processed
		UPDATE expiration_events
		SET processed = TRUE
		WHERE expires_at < datetime('now') AND processed = FALSE;
	END;

	-- Insert default configuration settings if they don't exist
	INSERT OR IGNORE INTO config_settings (setting_key, setting_value, setting_type, description) VALUES
		('max_queue_size', '50', 'integer', 'Maximum number of users allowed in queue'),
		('default_duration', '3h', 'duration', 'Default tag assignment duration'),
		('max_duration', '3h', 'duration', 'Maximum allowed tag assignment duration'),
		('min_duration', '30m', 'duration', 'Minimum allowed tag assignment duration'),
		('extension_time', '3h', 'duration', 'Duration to extend assignments by'),
		('expiration_check', '5m', 'duration', 'How often to check for expired assignments'),
		('auto_save_interval', '10m', 'duration', 'How often to auto-save data'),
		('slack_channel_id', '', 'string', 'Default Slack channel for notifications'),
		('slack_bot_token', '', 'string', 'Slack bot token'),
		('enable_notifications', 'true', 'string', 'Whether to send notifications');
	`

	_, err := db.conn.Exec(schema)
	return err
}

// QueueItem represents a user in the queue
type QueueItem struct {
	ID              int       `json:"id"`
	UserID          string    `json:"user_id"`
	Username        string    `json:"username"`
	Environment     string    `json:"environment"`
	Tag             string    `json:"tag"`
	DurationMinutes int       `json:"duration_minutes"`
	JoinedAt        time.Time `json:"joined_at"`
	Position        int       `json:"position"`
}

// Tag represents a bookable tag
type Tag struct {
	ID            int        `json:"id"`
	Name          string     `json:"name"`
	EnvironmentID int        `json:"environment_id"`
	Environment   string     `json:"environment"`
	Status        string     `json:"status"`
	AssignedTo    *string    `json:"assigned_to,omitempty"`
	AssignedAt    *time.Time `json:"assigned_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Environment represents a test environment
type Environment struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Tags      []Tag     `json:"tags"`
}
