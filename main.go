package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// keyManager manages the API keys, rotation, and failure handling.
type keyManager struct {
	mu              sync.Mutex
	keys            []string          // Original list of keys
	availableKeys   map[int]string    // Keys currently available for use (index -> key)
	failingKeys     map[int]time.Time // Keys currently failing (index -> reactivation time)
	currentIndex    uint64            // Atomic counter for round-robin index
	removalDuration time.Duration
}

// Context key type for associating the used key index with a request.
type contextKey string

const keyIndexContextKey contextKey = "keyIndex"

// newKeyManager creates and initializes a key manager.
func newKeyManager(keys []string, removalDuration time.Duration) (*keyManager, error) {
	if len(keys) == 0 {
		return nil, errors.New("at least one API key must be provided")
	}
	if removalDuration <= 0 {
		return nil, errors.New("key removal duration must be positive")
	}

	available := make(map[int]string, len(keys))
	for i, k := range keys {
		if k == "" {
			log.Printf("Warning: Empty key provided at index %d, skipping.", i)
			continue
		}
		available[i] = k
	}

	if len(available) == 0 {
		return nil, errors.New("no valid (non-empty) API keys found")
	}

	log.Printf("Initialized Key Manager with %d valid keys.", len(available))

	return &keyManager{
		keys:            keys, // Store original for index mapping
		availableKeys:   available,
		failingKeys:     make(map[int]time.Time),
		removalDuration: removalDuration,
		// currentIndex starts at 0 implicitly
	}, nil
}

// getNextKey selects the next available key using round-robin.
// It returns the key, its original index, and an error if none are available.
func (km *keyManager) getNextKey() (string, int, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	now := time.Now()

	// 1. Reactivate keys whose removal duration has passed
	for index, reactivateTime := range km.failingKeys {
		if now.After(reactivateTime) {
			log.Printf("Reactivating key index %d", index)
			km.availableKeys[index] = km.keys[index] // Use original key
			delete(km.failingKeys, index)
		}
	}

	// 2. Check if any keys are available
	if len(km.availableKeys) == 0 {
		log.Println("Error: No API keys currently available.")
		// Check if it's because *all* keys are temporarily failing
		if len(km.failingKeys) > 0 {
			earliestReactivation := time.Time{}
			for _, t := range km.failingKeys {
				if earliestReactivation.IsZero() || t.Before(earliestReactivation) {
					earliestReactivation = t
				}
			}
			return "", -1, errors.New("all keys are temporarily failing. Next reactivation: " + earliestReactivation.Format(time.RFC3339))
		}
		// Should not happen if validation passed, but safeguard
		return "", -1, errors.New("no keys configured or available")
	}

	// 3. Find the next available key using atomic round-robin
	numOriginalKeys := uint64(len(km.keys))
	if numOriginalKeys == 0 { // Should be caught earlier, but safety first
		return "", -1, errors.New("internal error: key list is empty")
	}

	// Try finding an available key starting from the current index
	// We loop max len(km.keys) times to ensure we check every possible slot
	// in case the available keys are sparse.
	startIndex := atomic.AddUint64(&km.currentIndex, 1) - 1 // Get current value and increment for next time
	for i := uint64(0); i < numOriginalKeys; i++ {
		currentIndex := (startIndex + i) % numOriginalKeys
		keyIndex := int(currentIndex)

		if key, ok := km.availableKeys[keyIndex]; ok {
			// Found an available key
			return key, keyIndex, nil
		}
	}

	// If we looped through all original indices and found nothing in availableKeys
	// (This implies availableKeys is empty, which should have been caught earlier)
	log.Println("Error: Could not find an available key despite availableKeys map not being empty (Concurrency issue?).")
	return "", -1, errors.New("no available key found after checking all indices")
}

// markKeyFailed temporarily removes a key from rotation.
func (km *keyManager) markKeyFailed(keyIndex int) {
	km.mu.Lock()
	defer km.mu.Unlock()

	// Only mark as failed if it's currently considered available
	if _, ok := km.availableKeys[keyIndex]; ok {
		reactivationTime := time.Now().Add(km.removalDuration)
		km.failingKeys[keyIndex] = reactivationTime
		delete(km.availableKeys, keyIndex)
		log.Printf("Marking key index %d as failing. Will reactivate around %s", keyIndex, reactivationTime.Format(time.RFC1123))
	} else {
		// It might already be marked as failing by another concurrent request
		log.Printf("Key index %d already marked as failing or is invalid.", keyIndex)
	}
}

func main() {
	// --- Command Line Flags ---
	targetHost := flag.String("target", "https://generativelanguage.googleapis.com", "Target host to forward requests to")
	listenAddr := flag.String("listen", ":8080", "Address and port to listen on")
	keysRaw := flag.String("keys", os.Getenv("GEMINI_API_KEYS"), "Comma-separated list of API keys (required)")
	removalDuration := flag.Duration("removal-duration", 5*time.Minute, "Duration to remove a failing key from rotation")
	overrideKeyParam := flag.String("key-param", "key", "The name of the query parameter containing the API key to override")

	flag.Parse()

	// --- Input Validation ---
	if *keysRaw == "" {
		log.Fatal("Error: -keys flag is required.")
	}
	keys := strings.Split(*keysRaw, ",")
	validKeys := []string{}
	for _, k := range keys {
		trimmedKey := strings.TrimSpace(k)
		if trimmedKey != "" {
			validKeys = append(validKeys, trimmedKey)
		}
	}
	if len(validKeys) == 0 {
		log.Fatal("Error: No non-empty API keys provided in the -keys flag.")
	}

	targetURL, err := url.Parse(*targetHost)
	if err != nil {
		log.Fatalf("Error parsing target host URL: %v", err)
	}
	if targetURL.Scheme == "" || targetURL.Host == "" {
		log.Fatalf("Error: Invalid target URL '%s'. Must include scheme (e.g., https://) and host.", *targetHost)
	}

	// --- Initialize Key Manager ---
	keyMan, err := newKeyManager(validKeys, *removalDuration)
	if err != nil {
		log.Fatalf("Error initializing key manager: %v", err)
	}

	// --- Create Reverse Proxy ---
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// --- Customize Proxy Director ---
	// The Director modifies the request *before* it's sent to the target.
	originalDirector := proxy.Director // Save original director if needed later
	proxy.Director = func(req *http.Request) {
		originalDirector(req) // Run the default director first (sets host, scheme, etc.)

		// Select the next API key
		apiKey, keyIndex, err := keyMan.getNextKey()
		if err != nil {
			// We cannot proceed without a key.
			// In a real-world scenario, you might want to return a 503 Service Unavailable
			// directly here, but modifying the request to cause an error downstream
			// might be simpler for httputil.ReverseProxy handling.
			// For now, we log and let the request potentially fail later.
			// A better approach would involve middleware *before* the proxy.
			log.Printf("Director Error: Could not get next key: %v", err)
			// Mark request as invalid? Add a specific header?
			// For simplicity, we'll let it proceed, likely failing at the target
			// or ModifyResponse if we add error handling there.
			// Or, we could add a custom response writer here to immediately return 503.
			// Let's add the error to context for ModifyResponse/ErrorHandler
			*req = *req.WithContext(context.WithValue(req.Context(), "proxyError", err)) // Store error in context
			return
		}

		log.Printf("Using key index %d for request to %s", keyIndex, req.URL.Path)

		// Store the used key index in the request context.
		// This allows ModifyResponse to know which key potentially failed.
		*req = *req.WithContext(context.WithValue(req.Context(), keyIndexContextKey, keyIndex))

		// Get existing query parameters
		query := req.URL.Query()
		// Override the specified key parameter
		query.Set(*overrideKeyParam, apiKey)
		// Encode the query parameters back into the URL
		req.URL.RawQuery = query.Encode()

		// Ensure the Host header is set correctly for the target host
		// NewSingleHostReverseProxy usually handles this, but explicit setting can be safer.
		req.Host = targetURL.Host
	}

	// --- Customize Proxy ModifyResponse ---
	// ModifyResponse is called *after* the backend response is received but *before*
	// it's sent back to the original client.
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Retrieve the key index used for this request from the context
		keyIndexVal := resp.Request.Context().Value(keyIndexContextKey)
		if keyIndexVal == nil {
			// No key index found, maybe an error occurred in the Director?
			// Or the request didn't need a key?
			proxyErrVal := resp.Request.Context().Value("proxyError")
			if proxyErr, ok := proxyErrVal.(error); ok {
				log.Printf("Error occurred during key selection for this request: %v", proxyErr)
				// Potentially modify the response to indicate a server-side proxy error (e.g., 503)
				// resp.StatusCode = http.StatusServiceUnavailable
				// resp.Status = http.StatusText(http.StatusServiceUnavailable)
				// // Clear body or set a custom error message?
			} else {
				log.Println("Warning: No key index found in request context for ModifyResponse.")
			}
			return nil // Return nil to avoid stopping the response proxying
		}

		keyIndex, ok := keyIndexVal.(int)
		if !ok {
			log.Printf("Error: Invalid key index type in context: %T", keyIndexVal)
			return nil // Don't stop proxying
		}

		// Check response status code for potential key-related failures
		// 429: Rate limited
		// 400/403: Could be invalid key, bad request format, or permissions issue.
		// We'll tentatively mark the key as failed for these common cases.
		// You might want to refine this logic based on the specific API's error responses.
		if resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusBadRequest ||
			resp.StatusCode == http.StatusForbidden {
			log.Printf("Request using key index %d failed with status %d. Marking key as failing.", keyIndex, resp.StatusCode)
			keyMan.markKeyFailed(keyIndex)
		}

		// Log other client/server errors from the target for debugging
		if resp.StatusCode >= 400 {
			log.Printf("Target responded with status %d for key index %d", resp.StatusCode, keyIndex)
		}

		return nil // Return nil to indicate success and continue proxying the response
	}

	// --- Customize Proxy ErrorHandler ---
	// ErrorHandler is called when the proxy encounters an error trying to connect
	// or communicate with the target server.
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		log.Printf("Proxy ErrorHandler triggered: %v", err)

		// Check if the error was due to no key being available from the start
		proxyErrVal := req.Context().Value("proxyError")
		if proxyErr, ok := proxyErrVal.(error); ok {
			http.Error(rw, "Proxy error: "+proxyErr.Error(), http.StatusServiceUnavailable)
			return
		}

		// Attempt to retrieve the key index if it was set before the error occurred
		keyIndexVal := req.Context().Value(keyIndexContextKey)
		if keyIndexVal != nil {
			if keyIndex, ok := keyIndexVal.(int); ok {
				// We *could* mark the key as failed here too, but network errors
				// don't necessarily mean the key itself is bad. It's safer to only
				// mark based on explicit API responses (like in ModifyResponse).
				log.Printf("Error occurred during request with key index %d: %v", keyIndex, err)
			}
		}

		// Default error handling: return Bad Gateway
		http.Error(rw, "Proxy Error: "+err.Error(), http.StatusBadGateway)
	}

	// --- Start HTTP Server ---
	log.Printf("Starting proxy server on %s", *listenAddr)
	log.Printf("Forwarding requests to %s", targetURL.String())
	log.Printf("Overriding query parameter '%s'", *overrideKeyParam)
	log.Printf("Key removal duration on failure: %s", *removalDuration)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Received request: %s %s%s", r.Method, r.Host, r.URL.RequestURI())
		// Allow CORS (adjust origins as needed for security)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")

		// Handle OPTIONS preflight requests
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	if err := http.ListenAndServe(*listenAddr, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

