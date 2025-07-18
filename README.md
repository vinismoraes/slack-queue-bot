# Slack Queue Bot

A Slack bot for managing test environment queues with automatic tag assignment and expiration.

## Features

- Queue management for test environments
- Automatic tag assignment and expiration (30-second intervals)
- User position tracking and wait time estimation
- Admin privilege system with visual indicators
- Interactive Slack UI with contextual buttons
- Silent status updates (no channel spam)
- Background expiration service for automatic cleanup

## Admin Setup

### Setting Up the First Admin

When setting up the bot for the first time, you need to configure at least one admin user:

1. **Get your Slack User ID**:
   - Go to your Slack profile
   - Click "More" → "Copy member ID"
   - Your user ID will look like `U095P7EPAT1`

2. **Set the bootstrap admin environment variable**:
   ```bash
   export ADMIN_USER_ID=U095P7EPAT1  # Replace with your actual user ID
   ```

3. **Start the bot**:
   ```bash
   ADMIN_USER_ID=U095P7EPAT1 go run main.go
   # or with the compiled binary:
   ADMIN_USER_ID=U095P7EPAT1 ./slack-queue-bot
   ```

4. **Verify admin access**:
   - Use `@bot help` in your Slack channel
   - You should see the "🔑 Admin Commands" section
   - Your status messages will show a crown (👑) indicator

### Managing Additional Admins

Once you have admin access, you can manage other admin users:

```bash
# List current admins
@bot manage-admins list

# Add a new admin (supports @mentions)
@bot manage-admins add @username
@bot manage-admins add U123456789

# Remove an admin
@bot manage-admins remove U123456789
```

**Note**: You cannot remove yourself if you're the only admin.

## Admin Features

Admins have access to additional commands and visual indicators:

### Visual Indicators
- 👑 Crown emoji next to admin names in status messages
- "👑 _Admin controls visible_" context in status displays
- Dedicated "🔑 Admin Commands" section in help

### Admin-Only Commands
- `@bot assign` - Manually assign next user in queue
- `@bot force-cleanup` - Force immediate cleanup of expired tags
- `@bot clear [queue|tags|all]` - Clear queue and/or release tags
- `@bot manage-admins [list|add|remove]` - Manage admin privileges

### Improved Command Names
For better clarity, the following aliases are available:
- `@bot force-cleanup` (alias for `cleanup`)
- `@bot clear` (alias for `clean`)
- `@bot manage-admins` (alias for `admin`)

## Configuration

The bot uses a database to store configuration settings for test environments, tags, and settings. Configuration is managed through database operations.

### Key Settings
- `max_queue_size`: Maximum number of users in queue
- `default_duration`: Default assignment duration
- `max_duration`: Maximum allowed assignment duration
- `expiration_check`: Background expiration check interval (30 seconds)
- `admin_users`: Admin user IDs (managed via bot commands)

## Development

```bash
# Install dependencies
go mod download

# Run the bot
go run main.go

# Build binary
go build -o slack-queue-bot .
```

## Environment Variables

- `SLACK_BOT_TOKEN`: Your Slack bot token
- `SLACK_APP_TOKEN`: Your Slack app token for socket mode
- `SLACK_CHANNEL_ID`: Channel ID where the bot operates
- `ADMIN_USER_ID`: Bootstrap admin user ID (first-time setup only)
- `DATA_DIR`: Directory for data storage (default: "./data")