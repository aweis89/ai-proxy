package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
)

// createProxyDirector returns a function that modifies the request before forwarding.
// It selects an API key, sets it in the query parameters, and handles key retrieval errors.
func createProxyDirector(keyMan *keyManager, targetURL *url.URL, overrideKeyParam string, originalDirector func(*http.Request)) func(*http.Request) {
	return func(req *http.Request) {
		originalDirector(req) // Run the default director first

		apiKey, keyIndex, err := keyMan.getNextKey()
		if err != nil {
			log.Printf("Director Error: Could not get next key: %v", err)
			// Add error to context for ErrorHandler
			*req = *req.WithContext(context.WithValue(req.Context(), proxyErrorContextKey, err))
			return
		}

		log.Printf("Using key index %d for request to %s", keyIndex, req.URL.Path)
		*req = *req.WithContext(context.WithValue(req.Context(), keyIndexContextKey, keyIndex))

		existingHeader := req.Header.Get("Authorization")
		fmt.Printf("Existing Authorization header: %s\n", existingHeader)
		fmt.Printf("Existing URL: %s\n", req.URL.String())

		if existingHeader != "" {
			// Set the Authorization header, replacing any existing one. Assuming Bearer token format.
			req.Header.Set("Authorization", "Bearer "+apiKey)
			fmt.Printf("Authorization header set to: %s\n", req.Header.Get("Authorization"))
		}
		if req.URL.Query().Get(overrideKeyParam) != "" {
			query := req.URL.Query()
			query.Set(overrideKeyParam, apiKey)
			req.URL.RawQuery = query.Encode()
			log.Printf("Outgoing request URL with key: %s\n", req.URL.String())
		}

		log.Println("--- Outgoing Request Headers ---")
		for name, values := range req.Header {
			for _, value := range values {
				log.Printf("%s: %s", name, value)
			}
		}
		log.Println("------------------------------")

		req.Host = targetURL.Host
	}
}

// createProxyModifyResponse returns a function that modifies the response from the target.
// It checks for specific status codes (429, 400, 403) and marks the used key as failed if necessary.
func createProxyModifyResponse(keyMan *keyManager) func(*http.Response) error {
	return func(resp *http.Response) error {
		keyIndexVal := resp.Request.Context().Value(keyIndexContextKey)

		// If no keyIndex was added (e.g., director failed), check for a proxyError
		if keyIndexVal == nil {
			proxyErrVal := resp.Request.Context().Value(proxyErrorContextKey)  // Use defined constant
			if proxyErr, ok := proxyErrVal.(error); ok && proxyErrVal != nil { // Explicit nil check
				log.Printf("Error occurred during key selection for this request: %v", proxyErr)
			} else {
				// Only log warning if there wasn't a proxyError either
				if proxyErrVal == nil {
					log.Println("Warning: No key index or proxy error found in request context for ModifyResponse.")
				}
			}
			return nil // Return early as there's no key index to process
		}

		keyIndex, ok := keyIndexVal.(int)
		if !ok {
			log.Printf("Error: Invalid key index type in context: %T", keyIndexVal)
			return nil
		}

		// Log response body for non-2xx status codes
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Printf("Request using key index %d received non-2xx status: %d", keyIndex, resp.StatusCode)
			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("Error reading non-2xx response body: %v", err)
				// Restore empty body if read fails
				resp.Body = io.NopCloser(bytes.NewBuffer(nil))
			} else {
				log.Printf("Non-2xx Response Body (Status %d)", resp.StatusCode)
				// Restore the body so the client can read it
				resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			}

			// Still mark key as failed only for >= 400 status codes
			if resp.StatusCode >= 400 {
				log.Printf("Marking key index %d as failing due to status %d.", keyIndex, resp.StatusCode)
				keyMan.markKeyFailed(keyIndex)
			}
		}

		return nil
	}
}

// createProxyErrorHandler returns a function that handles errors during proxying.
// It logs the error and returns an appropriate HTTP error status to the client.
func createProxyErrorHandler() func(http.ResponseWriter, *http.Request, error) {
	return func(rw http.ResponseWriter, req *http.Request, err error) {
		log.Printf("Proxy ErrorHandler triggered: %v", err)

		proxyErrVal := req.Context().Value(proxyErrorContextKey)
		if proxyErr, ok := proxyErrVal.(error); ok {
			http.Error(rw, "Proxy error: "+proxyErr.Error(), http.StatusServiceUnavailable)
			return
		}

		keyIndexVal := req.Context().Value(keyIndexContextKey)
		if keyIndexVal != nil {
			if keyIndex, ok := keyIndexVal.(int); ok {
				log.Printf("Error occurred during request with key index %d: %v", keyIndex, err)
			}
		}

		http.Error(rw, "Proxy Error: "+err.Error(), http.StatusBadGateway)
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
