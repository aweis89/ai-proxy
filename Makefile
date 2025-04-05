.PHONY: run logs build generate-launchctl-config

# Binary name
BINARY_NAME=ai-proxy

# Default log file path
LOG_FILE ?= $(HOME)/tmp/ai-proxy.log

run:
	@echo "Attempting to stop any existing proxy on port 8080..."
	@-lsof -ti :8080 | xargs kill > /dev/null 2>&1 || true
	@sleep 1 # Give the old process a moment to shut down
	@echo "Starting the AI proxy in the background..."
	@echo "Output will be logged to $(LOG_FILE)"
	@mkdir -p $(dir $(LOG_FILE))
	@nohup go run $(CURDIR)/. > $(LOG_FILE) 2>&1 &

build:
	@echo "Building $(BINARY_NAME)..."
	@go build -o $(CURDIR)/$(BINARY_NAME) $(CURDIR)/.

# You can override the log file path like this:
# make run LOG_FILE=/path/to/your/log/file.log

logs:
	@echo "Tailing log file: $(LOG_FILE)"
	@tail -f $(LOG_FILE)

# Generates a launchd plist file to run the proxy as a service
# Requires GEMINI_API_KEYS environment variable to be set
# Example: make generate-launchctl-config
generate-launchctl-config: build
	@echo "Generating launchctl configuration..."
	@if [ -z "$${GEMINI_API_KEYS}" ]; then \
		echo "Error: GEMINI_API_KEYS environment variable is not set."; \
		exit 1; \
	fi
	@PLIST_PATH="$(HOME)/Library/LaunchAgents/com.user.aiproxy.plist"; \
	echo "Ensuring directories exist..."; \
	mkdir -p $$(dirname "$${PLIST_PATH}"); \
	mkdir -p $$(dirname "$(LOG_FILE)"); \
	echo "Writing plist to $${PLIST_PATH}..."; \
	echo '<?xml version="1.0" encoding="UTF-8"?>' > "$${PLIST_PATH}"; \
	echo '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' >> "$${PLIST_PATH}"; \
	echo '<plist version="1.0">' >> "$${PLIST_PATH}"; \
	echo '<dict>' >> "$${PLIST_PATH}"; \
	echo '    <key>Label</key>' >> "$${PLIST_PATH}"; \
	echo '    <string>com.user.aiproxy</string>' >> "$${PLIST_PATH}"; \
	echo '    <key>ProgramArguments</key>' >> "$${PLIST_PATH}"; \
	echo '    <array>' >> "$${PLIST_PATH}"; \
	echo "        <string>$(CURDIR)/$(BINARY_NAME)</string>" >> "$${PLIST_PATH}"; \
	echo '        <string>-keys</string>' >> "$${PLIST_PATH}"; \
	echo "        <string>$${GEMINI_API_KEYS}</string>" >> "$${PLIST_PATH}"; \
	echo '    </array>' >> "$${PLIST_PATH}"; \
	echo '    <key>RunAtLoad</key>' >> "$${PLIST_PATH}"; \
	echo '    <true/>' >> "$${PLIST_PATH}"; \
	echo '    <key>KeepAlive</key>' >> "$${PLIST_PATH}"; \
	echo '    <true/>' >> "$${PLIST_PATH}"; \
	echo '    <key>StandardOutPath</key>' >> "$${PLIST_PATH}"; \
	echo "    <string>$(LOG_FILE)</string>" >> "$${PLIST_PATH}"; \
	echo '    <key>StandardErrorPath</key>' >> "$${PLIST_PATH}"; \
	echo "    <string>$(LOG_FILE)</string>" >> "$${PLIST_PATH}"; \
	echo '</dict>' >> "$${PLIST_PATH}"; \
	echo '</plist>' >> "$${PLIST_PATH}"; \
	echo ""; \
	echo "Attempting to unload existing agent (if any)..."; \
	launchctl unload "$${PLIST_PATH}" || true; \
	echo "Loading and enabling agent..."; \
	launchctl load -w "$${PLIST_PATH}"; \
	echo "Checking agent status (might take a moment to update)..."; \
	sleep 2; \
	launchctl print "gui/$$(id -u)/com.user.aiproxy" || echo "Agent status check failed or agent not running."; \
	echo ""; \
	echo "Service 'com.user.aiproxy' configured. Logs will be written to $(LOG_FILE)"
