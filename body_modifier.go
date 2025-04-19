package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"regexp"
)

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
