package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
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

		query := req.URL.Query()
		query.Set(overrideKeyParam, apiKey)
		req.URL.RawQuery = query.Encode()
		req.Header.Del("Authorization")

		log.Printf("Outgoing request URL with key: %s", req.URL.String())
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
		if keyIndexVal == nil {
			proxyErrVal := resp.Request.Context().Value("proxyError")
			if proxyErr, ok := proxyErrVal.(error); ok {
				log.Printf("Error occurred during key selection for this request: %v", proxyErr)
			} else {
				log.Println("Warning: No key index found in request context for ModifyResponse.")
			}
			return nil
		}

		keyIndex, ok := keyIndexVal.(int)
		if !ok {
			log.Printf("Error: Invalid key index type in context: %T", keyIndexVal)
			return nil
		}

		if resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusBadRequest ||
			resp.StatusCode == http.StatusForbidden {
			log.Printf("Request using key index %d failed with status %d. Marking key as failing.", keyIndex, resp.StatusCode)
			keyMan.markKeyFailed(keyIndex)
		}

		if resp.StatusCode >= 400 {
			log.Printf("Target responded with status %d for key index %d", resp.StatusCode, keyIndex)
			// Log response body for errors
			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("Error reading error response body: %v", err)
				// Restore empty body if read fails
				resp.Body = io.NopCloser(bytes.NewBuffer(nil))
			} else {
				log.Printf("Error Response Body: %s", string(bodyBytes))
				// Restore the body so the client can read it
				resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
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

// createMainHandler returns the main HTTP handler function.
// It logs requests, handles CORS, optionally modifies POST bodies, and forwards requests to the proxy.
func createMainHandler(proxy *httputil.ReverseProxy, addGoogleSearch bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Received request: %s %s%s", r.Method, r.Host, r.URL.RequestURI())

		// --- Log and Modify POST Request Body ---
		if r.Method == http.MethodPost && r.Body != nil {
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				log.Printf("Error reading request body: %v", err)
				http.Error(w, "Error reading request body", http.StatusInternalServerError)
				return
			}
			log.Printf("Original Request Body: %s", string(bodyBytes))

			// --- Conditionally Add Google Search Tool ---
			if addGoogleSearch {
				var requestData map[string]any // Use 'any' instead of 'interface{}'
				if err := json.Unmarshal(bodyBytes, &requestData); err != nil {
					log.Printf("Warning: Failed to parse request body as JSON: %v. Proceeding with original body.", err)
				} else {
					googleSearchTool := map[string]any{
						"google_search": map[string]any{},
					}
					googleSearchFound := false
					var toolsSlice []any

					// Check if 'tools' exists and is a slice
					if toolsVal, ok := requestData["tools"]; ok {
						if slice, ok := toolsVal.([]any); ok {
							toolsSlice = slice
							// Check if google_search tool already exists
							for _, tool := range toolsSlice {
								if toolMap, ok := tool.(map[string]any); ok {
									if _, exists := toolMap["google_search"]; exists {
										googleSearchFound = true
										log.Println("'google_search' tool already present in request.")
										break
									}
								}
							}
						} else {
							log.Printf("Warning: 'tools' field is not a slice, type is %T. Overwriting.", toolsVal)
							// Treat as if 'tools' doesn't exist or is invalid, will create new slice below
						}
					}

					// If google_search was not found, add it
					if !googleSearchFound {
						if toolsSlice == nil {
							// 'tools' didn't exist or wasn't a valid slice, create a new one
							log.Println("Creating 'tools' field with 'google_search'.")
							toolsSlice = []any{googleSearchTool}
						} else {
							// 'tools' existed and was a slice, append google_search
							log.Println("Appending 'google_search' tool to existing tools.")
							toolsSlice = append(toolsSlice, googleSearchTool)
						}
						requestData["tools"] = toolsSlice
					}

					// Marshal the potentially modified requestData
					modifiedBodyBytes, err := json.Marshal(requestData)
					if err != nil {
						log.Printf("Warning: Failed to marshal modified request body: %v. Proceeding with original body.", err)
					} else {
						log.Printf("Modified Request Body: %s", string(modifiedBodyBytes))
						bodyBytes = modifiedBodyBytes // Use the modified body
					}
				}
			}

			newBodyReader := bytes.NewReader(bodyBytes)
			r.Body = io.NopCloser(newBodyReader)
			r.ContentLength = int64(newBodyReader.Len())
			r.Header.Set("Content-Length", fmt.Sprintf("%d", r.ContentLength))
			log.Printf("Updated Content-Length to: %d", r.ContentLength)
		}

		// Allow CORS
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		proxy.ServeHTTP(w, r)
	}
}
