package main

import (
	"bytes"
	"context"
	"errors" // Added errors import
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
)

// createProxyDirector returns a function that modifies the request before forwarding.
// With the retryTransport handling key selection and auth, this director is simplified.
// It primarily ensures the default director logic (setting scheme, host, path) runs
// and sets the Host header correctly.
func createProxyDirector(targetURL *url.URL, originalDirector func(*http.Request)) func(*http.Request) {
	return func(req *http.Request) {
		// Run the original director provided by NewSingleHostReverseProxy
		// This sets req.URL.Scheme, req.URL.Host, and potentially req.URL.Path
		originalDirector(req)

		// Set the Host header to the target host. The retryTransport will handle auth.
		req.Host = targetURL.Host

		// No key selection or auth logic needed here anymore.
		// No context modification needed here (retryTransport handles keyIndexContextKey).
		// Logging of headers can be moved to retryTransport if needed per-attempt.
	}
}

// createProxyModifyResponse returns a function that modifies the response from the target.
// It checks for specific status codes and marks the used key as failed if necessary.
// This is still useful for handling non-retryable errors (like 400 Bad Request)
// or logging the final outcome. The retryTransport handles marking keys for retryable errors (like 429).
func createProxyModifyResponse(keyMan *keyManager) func(*http.Response) error {
	return func(resp *http.Response) error {
		// Get the key index used in the *last* attempt from the context set by retryTransport.
		keyIndexVal := resp.Request.Context().Value(keyIndexContextKey)
		keyIndex, keyIndexOk := keyIndexVal.(int)

		if !keyIndexOk {
			// This might happen if the request failed before the transport even ran (e.g., context canceled)
			// or if the transport failed to get a key initially.
			log.Println("Warning: No key index found in request context for ModifyResponse.")
			// Log non-2xx status even if key index is missing
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				log.Printf("Received non-2xx status: %d (Key Index Unknown)", resp.StatusCode)
				// Log body without key context
				logResponseBody(resp)
			}
			return nil // Return early as there's no key index to process further
		}

		// Log response body for non-2xx status codes, now with key index context
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Printf("Request using key index %d (last attempt) received non-2xx status: %d", keyIndex, resp.StatusCode)
			logResponseBody(resp) // Use helper to read/restore body

			// Mark key as failed for non-retryable client errors (4xx) that weren't handled by transport.
			// Transport handles 429. This handles things like 400, 401, 403 etc.
			// Avoid marking for 5xx here, as transport might retry those, and they aren't key-specific.
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				log.Printf("Marking key index %d as failing due to non-retryable client error status %d.", keyIndex, resp.StatusCode)
				keyMan.markKeyFailed(keyIndex)
			}
		}

		return nil
	}
}

// logResponseBody reads, logs, and restores the response body. Used for error logging.
func logResponseBody(resp *http.Response) {
	if resp.Body == nil || resp.Body == http.NoBody {
		log.Printf("Non-2xx Response (Status %d) had no body.", resp.StatusCode)
		return
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close() // Close original body reader
	if err != nil {
		log.Printf("Error reading non-2xx response body (Status %d): %v", resp.StatusCode, err)
		// Restore empty body if read fails
		resp.Body = io.NopCloser(bytes.NewBuffer(nil))
	} else {
		// Limit logged body size to avoid flooding logs
		logLimit := 512
		bodyString := string(bodyBytes)
		if len(bodyString) > logLimit {
			bodyString = bodyString[:logLimit] + "... (truncated)"
		}
		log.Printf("Non-2xx Response Body (Status %d): %s", resp.StatusCode, bodyString)
		// Restore the body so the client can read it
		resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}
}

// createProxyErrorHandler returns a function that handles terminal errors during proxying,
// typically errors returned by the custom transport after exhausting retries.
func createProxyErrorHandler() func(http.ResponseWriter, *http.Request, error) {
	return func(rw http.ResponseWriter, req *http.Request, err error) {
		log.Printf("Proxy ErrorHandler triggered after transport/retries: %v", err)

		// Log key index if available
		keyIndexVal := req.Context().Value(keyIndexContextKey)
		if keyIndex, ok := keyIndexVal.(int); ok {
			log.Printf("-> Last attempt used key index %d", keyIndex)
		} else {
			log.Printf("-> Key index for last attempt not found in context.")
		}

		// Check for specific error types to determine the response status code.
		var proxyErrWithStatus *proxyErrorWithStatus
		if errors.As(err, &proxyErrWithStatus) {
			// Use the status code from the error returned by the transport
			log.Printf("--> Responding to client with upstream status: %d", proxyErrWithStatus.StatusCode)
			http.Error(rw, err.Error(), proxyErrWithStatus.StatusCode)
		} else if errors.Is(err, context.Canceled) {
			// Client closed the connection
			log.Printf("--> Responding to client with status: %d (Context Canceled)", http.StatusRequestTimeout)
			http.Error(rw, "Client connection closed", http.StatusRequestTimeout) // 499 Client Closed Request is common
		} else {
			// Generic transport error (connection refused, DNS error, etc.)
			log.Printf("--> Responding to client with status: %d (Bad Gateway)", http.StatusBadGateway)
			// Use the message expected by the test for generic upstream failures
			http.Error(rw, "Proxy Error: Upstream server failed after retries", http.StatusBadGateway) // 502
		}
	}
}

// Compile the regex for matching Gemini model paths once
var geminiPathRegex = regexp.MustCompile(`^/v1beta/models/gemini-.*`)

// createMainHandler returns the main HTTP handler function.
// It logs requests, handles CORS, optionally modifies POST bodies for specific paths, and forwards requests to the proxy.
func createMainHandler(proxy *httputil.ReverseProxy, addGoogleSearch bool, searchTrigger string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Received request: %s %s%s", r.Method, r.Host, r.URL.RequestURI())

		// Handle CORS headers first
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Conditionally process POST request body for specific paths
		if r.Method == http.MethodPost && r.Body != nil && geminiPathRegex.MatchString(r.URL.Path) {
			log.Printf("Path %s matches Gemini pattern, processing POST body.", r.URL.Path)
			modifiedBody, err := handlePostBody(r.Body, addGoogleSearch, searchTrigger)
			if err != nil {
				log.Printf("Error processing request body for %s: %v", r.URL.Path, err)
				http.Error(w, "Error processing request body", http.StatusInternalServerError)
				return
			}

			// Update request with modified body only if it was processed
			newBodyReader := bytes.NewReader(modifiedBody)
			r.Body = io.NopCloser(newBodyReader)
			r.ContentLength = int64(len(modifiedBody))
			r.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
			log.Printf("Updated Content-Length to: %d for %s", r.ContentLength, r.URL.Path)
		} else if r.Method == http.MethodPost && r.Body != nil {
			log.Printf("Path %s does not match Gemini pattern, forwarding POST body unmodified.", r.URL.Path)
		}

		proxy.ServeHTTP(w, r)
	}
}
