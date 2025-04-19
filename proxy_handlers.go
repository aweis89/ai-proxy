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

		// Log response body for non-2xx status codes
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Printf("Request using key index %d received non-2xx status: %d", keyIndex, resp.StatusCode)
			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("Error reading non-2xx response body: %v", err)
				// Restore empty body if read fails
				resp.Body = io.NopCloser(bytes.NewBuffer(nil))
			} else {
				log.Printf("Non-2xx Response Body (Status %d): %s", resp.StatusCode, string(bodyBytes))
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

// handlePostBody processes the POST request body and returns the modified body and any error.
func handlePostBody(body io.ReadCloser, addGoogleSearch bool, searchTrigger string) ([]byte, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	log.Printf("Original Request Body: %s", string(bodyBytes))

	if !addGoogleSearch {
		return bodyBytes, nil
	}

	return modifyBodyWithGoogleSearch(bodyBytes, searchTrigger)
}

// modifyBodyWithGoogleSearch conditionally adds the Google Search tool and modifies the request body.
func modifyBodyWithGoogleSearch(bodyBytes []byte, searchTrigger string) ([]byte, error) {
	var requestData map[string]any
	if err := json.Unmarshal(bodyBytes, &requestData); err != nil {
		// Non-JSON body or parse error, return original
		log.Printf("Warning: Failed to parse request body as JSON: %v. Proceeding with original body.", err)
		return bodyBytes, nil
	}

	modified := false
	triggerFound := false
	hasFunctionDeclarations := false

	// --- Check for trigger word in message content ---
	// Assuming structure: {"contents": [{"parts": [{"text": "..."}]}]}
	if contents, ok := requestData["contents"].([]any); ok {
		for _, contentItem := range contents {
			if contentMap, ok := contentItem.(map[string]any); ok {
				if parts, ok := contentMap["parts"].([]any); ok {
					// Compile regex for word boundary matching (case-insensitive)
					// We compile it here in case searchTrigger changes, though it's passed as an arg
					// If performance becomes an issue with many parts, consider compiling once outside the loop
					triggerPattern := `(?i)\b` + regexp.QuoteMeta(searchTrigger) + `\b`
					triggerRegex, err := regexp.Compile(triggerPattern)
					if err != nil {
						log.Printf("Error compiling search trigger regex: %v. Falling back to simple contains.", err)
						// Fallback or handle error appropriately
						// For now, let's log and potentially skip regex matching for this part
						continue // Or use strings.Contains as a fallback
					}

					for _, partItem := range parts {
						if partMap, ok := partItem.(map[string]any); ok {
							if text, ok := partMap["text"].(string); ok {
								if triggerRegex.MatchString(text) {
									triggerFound = true
									log.Printf("Search trigger word '%s' found as whole word in message.", searchTrigger)
									break // Found in this part, break inner loop
								}
							}
						}
					}
				}
			}
			if triggerFound {
				break
			}
		}
	}

	// --- Check for functionDeclarations ---
	toolsVal, toolsExist := requestData["tools"]
	if toolsExist {
		// Check if tools is an array
		if toolsSlice, ok := toolsVal.([]any); ok {
			for _, tool := range toolsSlice {
				if toolMap, ok := tool.(map[string]any); ok {
					if _, fdExists := toolMap["functionDeclarations"]; fdExists {
						hasFunctionDeclarations = true
						log.Println("Found 'functionDeclarations' within tools array.")
						break // Found it, no need to check further
					}
				}
			}
		} else if toolsMap, ok := toolsVal.(map[string]any); ok {
			// Check if tools is a map (less common for function declarations, but handle just in case)
			if _, fdExists := toolsMap["functionDeclarations"]; fdExists {
				hasFunctionDeclarations = true
				log.Println("Found 'functionDeclarations' within tools map.")
			}
		}
	}

	googleSearchTool := map[string]any{
		"google_search": map[string]any{},
	}

	// --- Apply modification logic ---
	if triggerFound {
		// Force google_search, remove functionDeclarations
		log.Println("Trigger found: Ensuring 'google_search' tool exists and removing 'functionDeclarations'.")

		// Remove functionDeclarations if they exist within a map structure
		if toolsExist {
			if toolsMap, ok := toolsVal.(map[string]any); ok {
				if hasFunctionDeclarations {
					delete(toolsMap, "functionDeclarations")
					log.Println("Removed 'functionDeclarations'.")
					modified = true // Mark modified as we deleted something
					// If the map becomes empty after deletion, remove the tools key? Or leave empty map?
					// Let's leave it potentially empty for now. If it causes issues, we can remove it.
					// if len(toolsMap) == 0 {
					// 	delete(requestData, "tools")
					// }
				}
				// Check if google_search is already there (unlikely if FD was present, but check anyway)
				googleSearchAlreadyPresent := false
				if _, gsExists := toolsMap["google_search"]; gsExists {
					googleSearchAlreadyPresent = true
				}
				if !googleSearchAlreadyPresent {
					toolsMap["google_search"] = googleSearchTool["google_search"]
					log.Println("Added 'google_search' to existing tools map.")
					modified = true
				}
				requestData["tools"] = toolsMap // Ensure the map is updated
			} else if _, ok := toolsVal.([]any); ok {
				// Tools is an array. Replace it entirely with just google_search.
				log.Println("Replacing existing tools array with just 'google_search'.")
				requestData["tools"] = []any{googleSearchTool}
				modified = true
			} else {
				// Tools is some other type, overwrite it.
				log.Printf("Overwriting existing 'tools' field (type %T) with 'google_search'.", toolsVal)
				requestData["tools"] = []any{googleSearchTool}
				modified = true
			}
		} else {
			// Tools field doesn't exist, create it with google_search
			log.Println("Creating 'tools' field with 'google_search'.")
			requestData["tools"] = []any{googleSearchTool}
			modified = true
		}

	} else {
		// No trigger word found
		if hasFunctionDeclarations {
			// FunctionDeclarations exist, do nothing regarding tools
			log.Println("No trigger found and 'functionDeclarations' present. No tool modification needed.")
			// modified remains false
		} else {
			// No FunctionDeclarations, add google_search if not already present
			log.Println("No trigger found and no 'functionDeclarations'. Ensuring 'google_search' tool exists.")
			if toolsExist {
				googleSearchAlreadyPresent := false
				// Check if it's an array
				if toolsSlice, ok := toolsVal.([]any); ok {
					for _, tool := range toolsSlice {
						if toolMap, ok := tool.(map[string]any); ok {
							if _, exists := toolMap["google_search"]; exists {
								googleSearchAlreadyPresent = true
								break
							}
						}
					}
					if !googleSearchAlreadyPresent {
						log.Println("Appending 'google_search' to existing tools array.")
						requestData["tools"] = append(toolsSlice, googleSearchTool)
						modified = true
					} else {
						log.Println("'google_search' tool already present in tools array.")
					}
				} else if toolsMap, ok := toolsVal.(map[string]any); ok {
					// Tools is a map, add google_search if not present
					if _, gsExists := toolsMap["google_search"]; !gsExists {
						log.Println("Adding 'google_search' to existing tools map.")
						toolsMap["google_search"] = googleSearchTool["google_search"]
						requestData["tools"] = toolsMap // Update the map
						modified = true
					} else {
						log.Println("'google_search' tool already present in tools map.")
					}
				} else {
					// Tools is some other type, overwrite it.
					log.Printf("Overwriting existing 'tools' field (type %T) with 'google_search'.", toolsVal)
					requestData["tools"] = []any{googleSearchTool}
					modified = true
				}
			} else {
				// Tools field doesn't exist, create it
				log.Println("Creating 'tools' field with 'google_search'.")
				requestData["tools"] = []any{googleSearchTool}
				modified = true
			}
		}
	}

	// --- Marshal back to JSON if modified ---
	if !modified {
		log.Println("Request body not modified.")
		return bodyBytes, nil // Return original if no changes
	}

	modifiedBodyBytes, err := json.Marshal(requestData)
	if err != nil {
		// Return error, let handlePostBody decide how to handle marshal failure
		return nil, fmt.Errorf("failed to marshal modified request body: %w", err)
	}

	log.Printf("Modified Request Body: %s", string(modifiedBodyBytes))
	return modifiedBodyBytes, nil
}

// createMainHandler returns the main HTTP handler function.
// It logs requests, handles CORS, optionally modifies POST bodies, and forwards requests to the proxy.
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

		// Process POST request body if present
		if r.Method == http.MethodPost && r.Body != nil {
			modifiedBody, err := handlePostBody(r.Body, addGoogleSearch, searchTrigger)
			if err != nil {
				log.Printf("Error processing request body: %v", err)
				http.Error(w, "Error processing request body", http.StatusInternalServerError)
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
