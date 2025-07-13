# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Accelero is a GitOps-based Docker deployment automation tool that enables zero-downtime deployments. It listens for webhook triggers (typically from Docker registries) and automatically deploys new container versions by gracefully updating services and removing old containers.

## Build and Development Commands

### Building Binaries
```bash
# Build cross-platform binaries for all supported architectures
./scripts/build_binaries.sh
```

### Building Docker Images
```bash
# Build and publish multi-architecture Docker images
./scripts/build_docker_images.sh

# Build security-focused local image
docker build -t accelero:latest --build-arg TARGETARCH=amd64 .

# Run with security configuration
docker-compose -f docker-compose.security.yml up -d
```

### Testing
```bash
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run tests for a specific package
go test ./internal/service
go test ./internal/handler
go test ./internal/utils
```

### Running the Application
```bash
# Run locally (requires Docker daemon)
go run ./cmd/main.go

# Build and run binary
go build -o accelero ./cmd/main.go
./accelero
```

### Documentation
```bash
# Serve documentation locally (requires mkdocs)
mkdocs serve

# Build documentation
mkdocs build
```

## Architecture

### Core Components

- **`cmd/main.go`**: Application entry point with HTTP server setup, signal handling, and Docker client initialization
- **`internal/handler/`**: Webhook handling with worker pool for processing deployment requests
- **`internal/service/`**: Core deployment logic including zero-downtime container management and rollback capabilities
- **`internal/utils/`**: Utility functions for container operations and health checks
- **`internal/middleware/`**: HTTP middleware (primarily API key authentication)
- **`internal/git/`**: Git repository operations for fetching compose files
- **`internal/compose/`**: Docker Compose file parsing and service configuration
- **`internal/network/`**: Docker network management

### Key Features

- **Zero-downtime deployments**: Creates new containers before removing old ones
- **Health check integration**: Waits for container health checks before proceeding
- **Rollback capabilities**: Automatic rollback on deployment failures
- **Dynamic worker pool**: CPU-based worker scaling with configurable overrides
- **Resource cleanup**: Automated Docker resource cleanup routine
- **Performance optimization**: Adaptive queue sizing based on system resources
- **Memory leak prevention**: Automatic cleanup of old deployment statuses

### Environment Variables

Required environment variables (checked at startup):
- `REPO_URL`: Git repository URL containing docker-compose files
- `REPO_USERNAME`: Git repository username
- `REPO_TOKEN`: Git repository access token
- `COMPOSE_PATH`: Path to docker-compose file in repository
- `API_KEY`: **REQUIRED** - Secure API key for webhook authentication (application will not start without this)

Optional environment variables:
- `LOG_LEVEL`: Logging level (default: info)
- `LOG_FORMAT`: Log format - "json" or text (default: text)
- `DOCKER_SOCK`: Docker socket path (default: unix:///var/run/docker.sock)
- `WORKER_COUNT`: Number of worker goroutines (default: 2 * CPU cores, min: 2, max: 50)
- `QUEUE_SIZE`: Task queue buffer size (default: 15 * workers, min: 50, max: 1000)
- `STATUS_CLEANUP_INTERVAL`: How often to clean up old deployment statuses (default: 1h)
- `STATUS_MAX_AGE`: Maximum age before deployment status cleanup (default: 24h)

### Security Features

- **Secure Authentication**: Constant-time API key comparison prevents timing attacks
- **Input Validation**: Comprehensive payload validation with size limits (1MB max)
- **Directory Traversal Protection**: Secure file path handling prevents directory traversal attacks
- **Cryptographic Request IDs**: Secure random request ID generation
- **Request Logging**: Security-focused logging for monitoring and incident response

### Deployment Flow

1. Webhook receives deployment trigger
2. Repository is cloned to fetch latest compose configuration
3. Current service state is captured for potential rollback
4. New containers are created and started with updated image
5. Health checks are performed on new containers
6. Old containers are gracefully removed after new ones are healthy
7. On failure, automatic rollback to previous state occurs

### API Endpoints

- `POST /webhook`: Receives deployment webhooks (requires API key authentication)
- `GET /status`: Returns all deployment statuses
- `GET /status?id=<request_id>`: Returns specific deployment status
- `GET /status?stats=true`: Returns memory and performance statistics

### Testing Strategy

The codebase includes unit tests for core components:
- `webhook_test.go`: Webhook handler testing
- `service_test.go`: Service deployment logic testing  
- `utils_test.go`: Utility function testing