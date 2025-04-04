.PHONY: run

# Default log file path
LOG_FILE ?= $(HOME)/tmp/ai-proxy.log

run:
	@echo "Starting the AI proxy in the background..."
	@echo "Output will be logged to $(LOG_FILE)"
	@mkdir -p $(dir $(LOG_FILE))
	@nohup go run $(CURDIR)/main.go > $(LOG_FILE) 2>&1 &

# You can override the log file path like this:
# make run LOG_FILE=/path/to/your/log/file.log
