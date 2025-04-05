package main

import (
	"errors"
	"log"
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
