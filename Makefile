# Arker Development Makefile

.PHONY: dev dev-build dev-down prod prod-down test clean logs

# Development commands
dev: dev-down
	@echo "🚀 Starting development environment..."
	docker-compose -f docker-compose.dev.yml up --build

dev-build:
	@echo "🔨 Building development containers..."
	docker-compose -f docker-compose.dev.yml build --no-cache

dev-down:
	@echo "🛑 Stopping development environment..."
	docker-compose -f docker-compose.dev.yml down

dev-logs:
	@echo "📋 Showing development logs..."
	docker-compose -f docker-compose.dev.yml logs -f

# Production commands  
prod: prod-down
	@echo "🚀 Starting production environment..."
	docker-compose up -d

prod-down:
	@echo "🛑 Stopping production environment..."
	docker-compose down

prod-logs:
	@echo "📋 Showing production logs..."
	docker-compose logs -f

# Testing and utilities
test:
	@echo "🧪 Running tests..."
	go test ./... -count=1

clean:
	@echo "🧹 Cleaning up..."
	docker-compose -f docker-compose.dev.yml down -v
	docker-compose down -v
	docker system prune -f

# Database operations
db-connect:
	@echo "🔗 Connecting to database..."
	docker-compose -f docker-compose.dev.yml exec db psql -U user -d arker

db-reset:
	@echo "🗄️ Resetting database..."
	docker-compose -f docker-compose.dev.yml down -v
	docker-compose -f docker-compose.dev.yml up -d db

# Help
help:
	@echo "Available commands:"
	@echo "  dev         - Start development environment with live reload"
	@echo "  dev-build   - Build development containers from scratch"
	@echo "  dev-down    - Stop development environment"
	@echo "  dev-logs    - Show live development logs"
	@echo "  prod        - Start production environment"
	@echo "  prod-down   - Stop production environment"
	@echo "  prod-logs   - Show production logs"
	@echo "  test        - Run Go tests"
	@echo "  clean       - Clean up all containers and volumes"
	@echo "  db-connect  - Connect to development database"
	@echo "  db-reset    - Reset development database"
	@echo "  help        - Show this help message"
