.PHONY: run logs

# Default log file path
LOG_FILE ?= $(HOME)/tmp/ai-proxy.log

run:
	@echo "Attempting to stop any existing proxy on port 8080..."
	@-lsof -ti :8080 | xargs kill > /dev/null 2>&1 || true
	@sleep 1 # Give the old process a moment to shut down
	@echo "Starting the AI proxy in the background..."
	@echo "Output will be logged to $(LOG_FILE)"
	@mkdir -p $(dir $(LOG_FILE))
	@nohup go run $(CURDIR)/main.go > $(LOG_FILE) 2>&1 &

# You can override the log file path like this:
# make run LOG_FILE=/path/to/your/log/file.log

logs:
	@echo "Tailing log file: $(LOG_FILE)"
	@tail -f $(LOG_FILE)
