# AI Proxy - Gemini API Key Rotator

This project provides a simple HTTP reverse proxy that sits in front of a target API (defaulting to the Google Generative Language API - `generativelanguage.googleapis.com`). Its main features are:

*   **API Key Rotation:** Rotates through a list of provided API keys in a round-robin fashion for outgoing requests.
*   **Key Failure Handling:** Automatically removes keys from rotation for a configurable duration if the target API responds with specific error codes (e.g., 429 Too Many Requests, 400 Bad Request, 403 Forbidden).
*   **Query Parameter Injection:** Injects the selected API key into a specified query parameter (default `key`) for each request.
*   **Request Body Modification (Optional):** Can automatically add a `google_search` tool definition to the JSON body of outgoing POST requests if it's missing.
*   **CORS Handling:** Includes basic CORS headers.

## Prerequisites

*   **Go:** You need Go installed on your system (version 1.18 or later recommended due to generics usage, although this specific code might work with slightly older versions). Download from [https://go.dev/dl/](https://go.dev/dl/).
*   **API Keys:** You need one or more API keys for the target service (e.g., Gemini API keys).

## Configuration

The proxy is configured via command-line flags and environment variables:

*   **API Keys (`-keys` / `GEMINI_API_KEYS`):** **Required.** Provide a comma-separated list of your API keys.
    *   Command line: `-keys="key1,key2,key3"`
    *   Environment Variable: `export GEMINI_API_KEYS="key1,key2,key3"` (The `-keys` flag takes precedence if both are provided).
*   **Target Host (`-target`):** The backend API host to forward requests to.
    *   Default: `https://generativelanguage.googleapis.com`
*   **Listen Address (`-listen`):** The address and port the proxy should listen on.
    *   Default: `:8080`
*   **Key Removal Duration (`-removal-duration`):** How long a key is sidelined after a failure.
    *   Default: `5m` (5 minutes)
*   **Key Query Parameter (`-key-param`):** The name of the query parameter used to send the API key to the target.
    *   Default: `key`
*   **Add Google Search Tool (`-add-google-search`):** Whether to automatically add the `google_search` tool to POST request bodies.
    *   Default: `true`

Use the `-h` flag to see all options:
```bash
go run main.go -h
# or after building:
./ai-proxy -h
```

## Building and Running with Make

The included `Makefile` simplifies common tasks. The default log file location is `~/tmp/ai-proxy.log`.

*   **Build the binary:**
    ```bash
    make build
    ```
    This compiles the proxy and creates an executable file named `ai-proxy` in the project directory.

*   **Run the proxy (background):**
    ```bash
    # Make sure GEMINI_API_KEYS is set in your environment first!
    export GEMINI_API_KEYS="YOUR_KEY_1,YOUR_KEY_2"
    make run
    ```
    This command stops any existing process on port 8080, then starts the proxy in the background using `go run`. Output (stdout and stderr) is redirected to the log file (`~/tmp/ai-proxy.log` by default).
    *   **Note:** `make run` relies on the `GEMINI_API_KEYS` environment variable. It does not pass command-line flags directly.
    *   **Custom Log File:**
        ```bash
        make run LOG_FILE=/path/to/your/log/file.log
        ```

*   **View Logs:**
    ```bash
    make logs
    # Or if using a custom log file:
    make logs LOG_FILE=/path/to/your/log/file.log
    ```
    This command tails the log file.

*   **Generate macOS Launch Agent (Run as Service):**
    ```bash
    # Make sure GEMINI_API_KEYS is set in your environment first!
    export GEMINI_API_KEYS="YOUR_KEY_1,YOUR_KEY_2"
    make generate-launchctl-config
    ```
    This builds the binary and generates a `.plist` file in `~/Library/LaunchAgents/` to run the proxy as a background service managed by `launchd`.
    *   **Requires `GEMINI_API_KEYS`:** The environment variable *must* be set when running this command.
    *   Follow the output instructions to load/unload the service using `launchctl`. Logs will go to the path specified in the Makefile (default `~/tmp/ai-proxy.log`).

## Direct Usage (Without Make)

1.  **Set API Keys (Example):**
    ```bash
    export GEMINI_API_KEYS="YOUR_API_KEY_1,YOUR_API_KEY_2"
    ```
2.  **Run using `go run`:**
    ```bash
    go run main.go -listen :8080
    ```
    *(Add other flags as needed)*
3.  **Build and Run:**
    ```bash
    go build -o ai-proxy main.go
    ./ai-proxy -listen :8080
    ```
    *(Add other flags as needed)*

## How it Works

1.  The proxy listens for incoming HTTP requests.
2.  For each request, it asks the `keyManager` for the next available API key using a round-robin strategy.
3.  It modifies the request:
    *   Sets the target scheme, host, and path.
    *   Adds the selected API key to the specified query parameter (`key` by default).
    *   Removes any existing `Authorization` header.
    *   If it's a POST request and `-add-google-search=true`, it parses the JSON body, adds/overwrites the `tools` field with `[{"google_search":{}}]`, and updates the `Content-Length`.
4.  The request is forwarded to the target host.
5.  When the response comes back from the target:
    *   If the status code indicates a potential key issue (400, 403, 429), the `keyManager` is notified to temporarily mark the key used for that request as failing.
    *   The response is sent back to the original client.
6.  The `keyManager` periodically checks failing keys and makes them available again after the `removal-duration` has passed.
7.  If no keys are available (either initially or because all are temporarily failing), the proxy returns an error (likely 503 Service Unavailable).
