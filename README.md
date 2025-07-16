# Slack Queue Bot

A powerful Slack bot built in Go for managing test environment queues and tag assignments. The bot provides an intuitive interface for developers to join queues, book specific tags within environments, and automatically manages assignments with configurable durations.

## 🚀 Features

### Core Functionality
- **Queue Management**: Join/leave queues for specific environment tags
- **Automatic Assignment**: Next user in queue gets automatically assigned when tags become available
- **Configurable Environments & Tags**: Fully configurable via JSON configuration file
- **Duration Control**: Custom booking durations (30 minutes to 3 hours in 30-minute intervals)
- **Position Tracking**: See your position in queue and estimated wait time
- **Interactive Buttons**: Quick actions for join, leave, check position, and assign next

### Advanced Features
- **Rich Notifications**: Beautiful Slack block messages with emojis, summaries, and suggestions
- **Queue Limits**: Configurable queue size limits with warnings
- **Automatic Expiration**: Tags automatically expire after the specified duration
- **Extension Support**: Extend your tag assignment time
- **Early Release**: Release tags before expiration
- **Maintenance Mode**: Put tags in maintenance status

### Persistence & Reliability
- **Robust Storage**: JSON-based persistence with automatic saving
- **Periodic Backups**: Automatic state backups every 10 minutes
- **Manual Save Endpoint**: HTTP endpoint for manual state saving
- **Startup Recovery**: Automatically loads previous state on startup

### Visual Status Display
- **Environment Overview**: Compact, visual display of all environments and tags
- **Status Indicators**: Color-coded tag status (available, occupied, maintenance)
- **Queue Information**: Queue size, limits, and estimated wait times
- **User Details**: Shows assigned user and time remaining for occupied tags

## 📋 Commands

### Text Commands
- `@bot join <environment> <tag> [duration]` - Join queue for a specific tag
- `@bot leave` - Leave the queue
- `@bot status` - Show current queue and environment status
- `@bot position` - Show your position in queue and estimated wait time
- `@bot assign` - Manually assign next user (admin only)
- `@bot extend <environment> <tag>` - Extend your tag assignment
- `@bot release <environment> <tag>` - Release your tag early
- `@bot list` - List all available environments and tags

### Interactive Actions
- **Quick Join** - Join queue with one click
- **Quick Leave** - Leave queue with one click
- **Check Position** - See your queue position instantly
- **Assign Next** - Manually assign next user (admin only)

## 🛠️ Installation & Setup

### Prerequisites
- Go 1.21 or higher
- Slack App with Socket Mode enabled
- Bot token and app token from Slack

### Environment Variables
Create a `.env` file based on `env.example`:

```bash
# Slack Bot Configuration
SLACK_BOT_TOKEN=xoxb-your-bot-token-here
SLACK_APP_TOKEN=xapp-your-app-token-here
SLACK_CHANNEL_ID=C1234567890

# Data persistence directory (optional, defaults to ./data)
DATA_DIR=./data

# Configuration file path (optional, defaults to ./config.json)
CONFIG_PATH=./config.json
```

### Configuration
The bot uses a JSON configuration file (`config.json`) to define environments and tags:

```json
{
  "environments": [
    {
      "name": "test-environment",
      "tags": ["api", "web", "mobile", "database"]
    }
  ],
  "settings": {
    "max_queue_size": 50,
    "default_duration": "3h",
    "max_duration": "3h",
    "min_duration": "30m",
    "extension_time": "3h",
    "expiration_check": "5m",
    "auto_save_interval": "10m"
  }
}
```

### Quick Start

#### Using Make
```bash
# Install dependencies and build
make all

# Run locally
make run

# Build binary
make build
```

#### Using Go directly
```bash
# Install dependencies
go mod download
go mod tidy

# Run the application
go run main.go

# Build binary
go build -o bin/slack-queue-bot main.go
```

#### Using Docker
```bash
# Build and run with Docker Compose
make docker-run

# Or run in background
make docker-run-detached

# Stop the application
make docker-stop
```

## 🐳 Docker Deployment

### Docker Compose
The project includes a complete Docker setup:

```bash
# Build and run
docker-compose up --build

# Run in background
docker-compose up -d --build

# View logs
docker-compose logs -f

# Stop
docker-compose down
```

### Manual Docker Build
```bash
# Build image
docker build -t slack-queue-bot .

# Run container
docker run -d \
  --name slack-queue-bot \
  -p 8080:8080 \
  --env-file .env \
  slack-queue-bot
```

## 📊 Configuration Options

### Environment Settings
- **max_queue_size**: Maximum number of users allowed in queue (default: 50)
- **default_duration**: Default booking duration (default: 3h)
- **max_duration**: Maximum allowed booking duration (default: 3h)
- **min_duration**: Minimum allowed booking duration (default: 30m)
- **extension_time**: How long to extend assignments (default: 3h)
- **expiration_check**: How often to check for expired assignments (default: 5m)
- **auto_save_interval**: How often to auto-save state (default: 10m)

### Duration Format
Durations can be specified in various formats:
- `30m` - 30 minutes
- `1h` - 1 hour
- `2h30m` - 2 hours 30 minutes
- `3h` - 3 hours

## 🔧 API Endpoints

The bot exposes HTTP endpoints for monitoring and management:

- `GET /health` - Health check endpoint
- `POST /save` - Manual state save trigger

## 📁 Project Structure

```
slack-queue-bot/
├── main.go              # Main application file
├── config.json          # Environment and tag configuration
├── env.example          # Environment variables template
├── go.mod               # Go module dependencies
├── go.sum               # Go module checksums
├── Makefile             # Build and deployment commands
├── Dockerfile           # Docker container definition
├── docker-compose.yml   # Docker Compose configuration
├── setup.sh             # Setup script
├── README.md            # This file
├── IMPROVEMENTS.md      # Development improvements log
├── bin/                 # Build output directory
└── data/                # Data persistence directory (created at runtime)
```

## 🔒 Security & Permissions

### Required Slack Permissions
- `app_mentions:read` - Read mentions of the bot
- `chat:write` - Send messages to channels
- `im:write` - Send direct messages to users
- `channels:read` - Read channel information
- `users:read` - Read user information

### Bot Token Scopes
- `app_mentions:read`
- `chat:write`
- `im:write`
- `channels:read`
- `users:read`

## 🚨 Error Handling

The bot provides comprehensive error handling with:
- **Rich Error Messages**: Detailed error descriptions with suggestions
- **Slack Block Formatting**: Beautiful error notifications with emojis
- **Graceful Degradation**: Continues operation even if some features fail
- **Automatic Recovery**: Attempts to recover from temporary failures

## 📈 Monitoring & Logging

- **Structured Logging**: All operations are logged with timestamps
- **State Persistence**: Automatic saving prevents data loss
- **Health Checks**: Docker health checks ensure service availability
- **Error Tracking**: Comprehensive error logging for debugging

## 🤝 Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests if applicable
5. Submit a pull request

## 📝 License

This project is licensed under the MIT License - see the LICENSE file for details.

## 🆘 Support

For issues and questions:
1. Check the configuration and environment variables
2. Review the logs for error messages
3. Ensure Slack permissions are correctly set
4. Verify the bot is invited to the target channel

## 🔄 Recent Updates

- **Interactive Buttons**: Added quick action buttons for common operations
- **Queue Position Tracking**: Users can see their position and estimated wait time
- **Rich Notifications**: Enhanced error messages and status displays
- **Configurable Environments**: Removed hardcoded "league" references
- **Queue Limits**: Added configurable queue size limits with warnings
- **Visual Improvements**: Enhanced environment/tag display with better formatting