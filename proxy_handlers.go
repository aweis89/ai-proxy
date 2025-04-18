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

		if resp.StatusCode >= 400 {
			log.Printf("Request using key index %d failed with status %d. Marking key as failing.", keyIndex, resp.StatusCode)
			keyMan.markKeyFailed(keyIndex)
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

// handlePostBody processes the POST request body and returns the modified body and any error.
func handlePostBody(body io.ReadCloser, addGoogleSearch bool) ([]byte, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	log.Printf("Original Request Body: %s", string(bodyBytes))

	if !addGoogleSearch {
		return bodyBytes, nil
	}

	return modifyBodyWithGoogleSearch(bodyBytes)
}

// modifyBodyWithGoogleSearch adds the Google Search tool to the request body if needed.
func modifyBodyWithGoogleSearch(bodyBytes []byte) ([]byte, error) {
	var requestData map[string]any
	if err := json.Unmarshal(bodyBytes, &requestData); err != nil {
		log.Printf("Warning: Failed to parse request body as JSON: %v. Proceeding with original body.", err)
		return bodyBytes, nil
	}

	googleSearchTool := map[string]any{
		"google_search": map[string]any{},
	}

	// Call addGoogleSearchToTools and capture the returned slice
	newToolsSlice, modified := addGoogleSearchToTools(requestData, googleSearchTool)
	if !modified {
		log.Println("No modification needed for tools.")
		return bodyBytes, nil // No changes needed
	}

	// If modified, we need to ensure requestData["tools"] is correctly set.
	// Check if the 'tools' field in the original data was NOT the map structure
	// that gets modified in place by addGoogleSearchToTools.
	if toolsVal, ok := requestData["tools"]; ok {
		if _, isMap := toolsVal.(map[string]any); !isMap {
			// If 'tools' existed but wasn't the map structure (i.e., it was a direct array),
			// update it with the new slice returned by the function.
			log.Println("Updating existing 'tools' array.")
			requestData["tools"] = newToolsSlice
		}
		// If it *was* the map structure, addGoogleSearchToTools modified it in place,
		// so no assignment is needed here. The log inside addGoogleSearchToTools covers this.
	} else {
		// If 'tools' field didn't exist originally, assign the new slice.
		log.Println("Assigning newly created 'tools' field.")
		requestData["tools"] = newToolsSlice
	}

	modifiedBodyBytes, err := json.Marshal(requestData)
	if err != nil {
		// It's generally better to return the error here so the caller knows marshaling failed.
		return nil, fmt.Errorf("failed to marshal modified request body: %w", err)
	}

	log.Printf("Modified Request Body: %s", string(modifiedBodyBytes))
	return modifiedBodyBytes, nil
}

// addGoogleSearchToTools handles the addition of Google Search tool to the tools section.
func addGoogleSearchToTools(requestData map[string]any, googleSearchTool map[string]any) ([]any, bool) {
	var toolsSlice []any
	googleSearchFound := false

	if toolsVal, ok := requestData["tools"]; ok {
		// Handle array format (direct tools array)
		if slice, ok := toolsVal.([]any); ok {
			toolsSlice = slice
			for _, tool := range toolsSlice {
				if toolMap, ok := tool.(map[string]any); ok {
					if _, exists := toolMap["google_search"]; exists {
						googleSearchFound = true
						log.Println("'google_search' tool already present in request.")
						break
					}
				}
			}
		} else if toolsMap, ok := toolsVal.(map[string]any); ok {
			// Handle object format with functionDeclarations
			if declarations, ok := toolsMap["functionDeclarations"].([]any); ok {
				toolsSlice = declarations
				for _, decl := range declarations {
					if declMap, ok := decl.(map[string]any); ok {
						if name, ok := declMap["name"].(string); ok && name == "google_search" {
							googleSearchFound = true
							log.Println("'google_search' tool already present in request.")
							break
						}
					}
				}

				// If we need to add the tool and not found
				if !googleSearchFound {
					log.Println("Adding 'google_search' to existing functionDeclarations.")
					// Convert our google_search tool to a functionDeclaration format
					googleSearchDecl := map[string]any{
						"name":        "google_search",
						"description": "Search Google for real-time information",
						"parameters": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"query": map[string]any{
									"type":        "string",
									"description": "The search query",
								},
							},
							"required": []string{"query"},
						},
					}
					toolsMap["functionDeclarations"] = append(declarations, googleSearchDecl)
					return toolsSlice, true // We return the original slice, but the modification is in the map
				}
				return toolsSlice, false
			}
		}
	}

	if googleSearchFound {
		return toolsSlice, false
	}

	if toolsSlice == nil {
		log.Println("Creating 'tools' field with 'google_search'.")
		return []any{googleSearchTool}, true
	}

	log.Println("Appending 'google_search' tool to existing tools.")
	return append(toolsSlice, googleSearchTool), true
}

// createMainHandler returns the main HTTP handler function.
// It logs requests, handles CORS, optionally modifies POST bodies, and forwards requests to the proxy.
func createMainHandler(proxy *httputil.ReverseProxy, addGoogleSearch bool) http.HandlerFunc {
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

		// Process POST request body if present
		if r.Method == http.MethodPost && r.Body != nil {
			modifiedBody, err := handlePostBody(r.Body, addGoogleSearch)
			if err != nil {
				log.Printf("Error reading request body: %v", err)
				http.Error(w, "Error reading request body", http.StatusInternalServerError)
				return
			}

			// Update request with modified body
			newBodyReader := bytes.NewReader(modifiedBody)
			r.Body = io.NopCloser(newBodyReader)
			r.ContentLength = int64(len(modifiedBody))
			r.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
			log.Printf("Updated Content-Length to: %d", r.ContentLength)
		}

		proxy.ServeHTTP(w, r)
	}
}
