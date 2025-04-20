package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	// --- Command Line Flags ---
	targetHost := flag.String("target", "https://generativelanguage.googleapis.com", "Target host to forward requests to")
	listenAddr := flag.String("listen", ":8080", "Address and port to listen on")
	keysRaw := flag.String("keys", os.Getenv("GEMINI_API_KEYS"), "Comma-separated list of API keys (required)")
	removalDuration := flag.Duration("removal-duration", 1*time.Hour, "Duration to remove a failing key from rotation")
	overrideKeyParam := flag.String("key-param", "key", "The name of the query parameter containing the API key to override")
	headerAuthPathsRaw := flag.String("header-auth-paths", "/openai", "Comma-separated list of path prefixes that should use Authorization header instead of query param")
	addGoogleSearch := flag.Bool("add-google-search", true, "Automatically add google_search tool based on conditions")
	searchTrigger := flag.String("search-trigger", "search", "Word in user message that forces google_search and removes functionDeclarations")

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

	// Process header auth paths
	headerAuthPaths := []string{}
	if *headerAuthPathsRaw != "" {
		for _, p := range strings.Split(*headerAuthPathsRaw, ",") {
			trimmedPath := strings.TrimSpace(p)
			if trimmedPath != "" {
				headerAuthPaths = append(headerAuthPaths, trimmedPath)
			}
		}
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

	// --- Customize Proxy ---
	originalDirector := proxy.Director // Save original director
	proxy.Director = createProxyDirector(keyMan, targetURL, *overrideKeyParam, headerAuthPaths, originalDirector)
	proxy.ModifyResponse = createProxyModifyResponse(keyMan)
	proxy.ErrorHandler = createProxyErrorHandler()

	// --- Start HTTP Server ---
	log.Printf("Starting proxy server on %s", *listenAddr)
	log.Printf("Forwarding requests to %s", targetURL.String())
	log.Printf("Using query parameter '%s' for API key (default)", *overrideKeyParam)
	if len(headerAuthPaths) > 0 {
		log.Printf("Using Authorization header for paths starting with: %v", headerAuthPaths)
	}
	log.Printf("Key removal duration on failure: %s", *removalDuration)
	log.Printf("Add google_search tool conditionally: %t", *addGoogleSearch)
	if *addGoogleSearch {
		log.Printf("Search trigger word: '%s'", *searchTrigger)
	}

	// --- Register Handler ---
	http.HandleFunc("/", createMainHandler(proxy, *addGoogleSearch, *searchTrigger))

	// --- Run Server ---
	if err := http.ListenAndServe(*listenAddr, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
