package main

import (
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"time"
)

// scopeState holds the state for a specific host+path combination.
type scopeState struct {
	// map of original key index -> key string for keys currently available for this scope
	availableKeys map[int]string
	// map of original key index -> reactivation time for keys currently failing for this scope
	failingKeys map[int]time.Time
	// round-robin index for this scope
	// We store it per-scope to avoid needing a global counter,
	// but we don't actually use it for selection anymore (we use random).
	// Could potentially be removed or repurposed.
	currentIndex int
}

// keyManager manages the API keys, rotation, and failure handling per scope.
type keyManager struct {
	mu sync.Mutex
	// Original list of keys, used for indexing and reactivation.
	originalKeys []string
	// Map of scope (host+path) -> scopeState
	scopes map[string]*scopeState
	// Default duration a key is sidelined after failure in a scope.
	removalDuration time.Duration
}

// Context key type for associating values with a request.
type contextKey string

const (
	keyIndexContextKey   contextKey = "keyIndex"
	proxyErrorContextKey contextKey = "proxyError"
)

// newKeyManager creates and initializes a key manager.
func newKeyManager(keys []string, removalDuration time.Duration) (*keyManager, error) {
	if len(keys) == 0 {
		return nil, errors.New("at least one API key must be provided")
	}
	if removalDuration <= 0 {
		return nil, errors.New("key removal duration must be positive")
	}

	// Validate keys - count valid ones
	validKeyCount := 0
	for i, k := range keys {
		if k == "" {
			log.Printf("Warning: Empty key provided at index %d, skipping.", i)
		} else {
			validKeyCount++
		}
	}

	if validKeyCount == 0 {
		return nil, errors.New("no valid (non-empty) API keys found")
	}

	log.Printf("Initialized Key Manager with %d valid keys. Scopes will be created on demand.", validKeyCount)

	km := &keyManager{
		originalKeys:    keys,
		scopes:          make(map[string]*scopeState),
		removalDuration: removalDuration,
	}

	// Start background goroutine for reactivating keys
	go km.reactivationLoop()

	return km, nil
}

// getOrCreateScopeState returns the scopeState for a given scope string,
// creating it if it doesn't exist.
// This function MUST be called with the keyManager mutex held.
func (km *keyManager) getOrCreateScopeState(scope string) *scopeState {
	if state, exists := km.scopes[scope]; exists {
		return state
	}

	// Scope doesn't exist, create it.
	newState := &scopeState{
		availableKeys: make(map[int]string),
		failingKeys:   make(map[int]time.Time),
		currentIndex:  0, // Initialize index
	}

	// Populate availableKeys with all *valid* original keys
	for i, key := range km.originalKeys {
		if key != "" { // Only add non-empty keys
			newState.availableKeys[i] = key
		}
	}

	km.scopes[scope] = newState
	log.Printf("Created new scope state for: %s with %d initial available keys", scope, len(newState.availableKeys))
	return newState
}

// buildScopeKey creates the key for the scopes map.
func buildScopeKey(host, path string) string {
	// Simple concatenation might be okay, but consider edge cases
	// like empty host or path if that's possible in your setup.
	// Using a separator ensures uniqueness if path could start with host chars.
	return fmt.Sprintf("%s|%s", host, path)
}

func (km *keyManager) getNextKey(scope string) (string, int, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	numOriginalKeys := uint64(len(km.originalKeys))
	if numOriginalKeys == 0 {
		log.Println("Error: Original key list is empty in getNextKey.")
		return "", -1, errors.New("internal error: key list is empty")
	}

	state := km.getOrCreateScopeState(scope)

	// 1. Check if any keys are available *in this scope*
	if len(state.availableKeys) == 0 {
		// Count how many *valid* original keys exist.
		validOriginalKeyCount := 0
		for _, k := range km.originalKeys {
			if k != "" {
				validOriginalKeyCount++
			}
		}

		// Check if the reason for no available keys is that all *valid* original keys are currently failing *in this scope*.
		if len(state.failingKeys) > 0 && len(state.failingKeys) == validOriginalKeyCount {
			// If we reach here, it means all *valid* original keys are temporarily failing *in this scope*.
			// Let's perform an immediate reactivation check for *this scope*.
			log.Printf("Scope '%s': All valid keys temporarily failing. Performing immediate reactivation check for this scope.", scope)
			keysReactivated := km.reactivateScopeKeys(state) // Call helper to reactivate expired keys in this scope
			log.Printf("Scope '%s': Immediate check reactivated %d keys.", scope, keysReactivated)

			// After attempting reactivation, check availability again.
			if len(state.availableKeys) == 0 {
				// If still no keys available after check, return the error.
				log.Printf("Scope '%s': Still no keys available after immediate reactivation check.", scope)
				return "", -1, fmt.Errorf("scope '%s': all keys are temporarily rate limited or failing", scope)
			} // else, proceed to select a key below
		} else { // This means len(state.availableKeys) == 0, but it's NOT because all valid keys are failing.
			// This could happen if all keys were initially empty or if somehow
			// availableKeys became empty without failingKeys reflecting it (shouldn't happen often).
			log.Printf("Error: Scope '%s': No API keys currently available, and not all valid keys were failing (Available: 0, Failing: %d, Valid Original: %d).", scope, len(state.failingKeys), validOriginalKeyCount)
			return "", -1, fmt.Errorf("scope '%s': no keys configured or available", scope)
		}
	} // End of outer check: if len(state.availableKeys) == 0 initially

	// 2. Find the next available key using random start within the original key indices
	startIndex := rand.IntN(int(numOriginalKeys)) // Generate a random starting index
	for i := range int(numOriginalKeys) {
		currentIndex := (startIndex + i) % int(numOriginalKeys)
		keyIndex := currentIndex

		if key, ok := state.availableKeys[keyIndex]; ok {
			// Found an available key for this scope
			log.Printf("Scope '%s': Selected key index %d. Available keys remaining in scope: %d", scope, keyIndex, len(state.availableKeys))
			return key, keyIndex, nil
		}
	}

	// Should be unreachable if len(state.availableKeys) > 0
	log.Printf("Error: Scope '%s': Could not find an available key despite availableKeys map (len %d) not being empty (Concurrency issue?). Failing keys: %d", scope, len(state.availableKeys), len(state.failingKeys))
	return "", -1, fmt.Errorf("scope '%s': no available key found after checking all indices", scope)
}

// markKeyFailed temporarily removes a key from rotation *for a specific scope*.
func (km *keyManager) markKeyFailed(scope string, keyIndex int) {
	km.mu.Lock()
	defer km.mu.Unlock()

	state := km.getOrCreateScopeState(scope)

	// Only mark as failed if it's currently considered available *in this scope*
	if _, ok := state.availableKeys[keyIndex]; ok {
		reactivationTime := time.Now().Add(km.removalDuration)
		state.failingKeys[keyIndex] = reactivationTime
		delete(state.availableKeys, keyIndex)
		log.Printf("Scope '%s': Marking key index %d as failing. Will reactivate around %s", scope, keyIndex, reactivationTime.Format(time.RFC1123))
	} else {
		// It might already be marked as failing by another concurrent request for this scope,
		// or the keyIndex might be invalid (e.g., for an initially empty key slot)
		if _, failing := state.failingKeys[keyIndex]; !failing {
			// Only log if it's not already known to be failing
			log.Printf("Scope '%s': Key index %d is not currently available; cannot mark as failing.", scope, keyIndex)
		}
	}
}

// reactivationLoop runs in the background to reactivate keys whose removal duration has passed.
func (km *keyManager) reactivationLoop() {
	// Check more frequently than the removal duration, e.g., every minute
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	log.Println("Key reactivation loop started.")

	for range ticker.C {
		km.reactivateKeys()
	}
}

// reactivateScopeKeys checks and reactivates keys for a *single given scope*.
// This MUST be called with the keyManager mutex held.
func (km *keyManager) reactivateScopeKeys(state *scopeState) int {
	now := time.Now()
	keysReactivated := 0
	scopeIdentifier := "<unknown scope>" // Placeholder
	// Find the scope string for logging (inefficient, but only used in error/reactivation paths)
	for s, st := range km.scopes {
		if st == state {
			scopeIdentifier = s
			break
		}
	}

	for index, reactivateTime := range state.failingKeys {
		if now.After(reactivateTime) {
			// Ensure the index is valid for the original key list and the key wasn't initially empty
			if index >= 0 && index < len(km.originalKeys) && km.originalKeys[index] != "" {
				log.Printf("Scope '%s': Reactivating key index %d (immediate check)", scopeIdentifier, index)
				state.availableKeys[index] = km.originalKeys[index]
				delete(state.failingKeys, index)
				keysReactivated++
			} else {
				log.Printf("Scope '%s': Removing invalid/empty key index %d from failing list (immediate check).", scopeIdentifier, index)
				delete(state.failingKeys, index)
			}
		}
	}
	return keysReactivated
}

// reactivateKeys checks all scopes and reactivates keys within each scope if their time is up.
func (km *keyManager) reactivateKeys() {
	km.mu.Lock()
	defer km.mu.Unlock()

	now := time.Now()
	// log.Println("Running periodic key reactivation check...") // Debug log

	for scope, state := range km.scopes {
		keysReactivatedInScope := 0
		for index, reactivateTime := range state.failingKeys {
			if now.After(reactivateTime) {
				// Ensure the index is valid for the original key list
				if index >= 0 && index < len(km.originalKeys) && km.originalKeys[index] != "" {
					log.Printf("Scope '%s': Reactivating key index %d", scope, index)
					state.availableKeys[index] = km.originalKeys[index] // Add back to available
					delete(state.failingKeys, index)                    // Remove from failing
					keysReactivatedInScope++
				} else {
					// This case handles invalid indices or indices corresponding to initially empty keys.
					// Just remove it from the failing map for this scope.
					log.Printf("Scope '%s': Removing invalid/empty key index %d from failing list.", scope, index)
					delete(state.failingKeys, index)
				}
			}
		}
	}
}
