package main

import (
	"errors"
	"log"
	"math/rand/v2"
	"sync"
	"time"
)

// keyManager manages the API keys, rotation, and failure handling.
type keyManager struct {
	mu              sync.Mutex
	keys            []string          // Original list of keys
	availableKeys   map[int]string    // Keys currently available for use (index -> key)
	failingKeys     map[int]time.Time // Keys currently failing (index -> reactivation time)
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
	}, nil
}

func (km *keyManager) getNextKey() (string, int, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	// Check if the original key list is empty *first*
	numOriginalKeys := uint64(len(km.keys))
	if numOriginalKeys == 0 { // Should be caught by constructor, but safety first
		log.Println("Error: Key list is empty in getNextKey.")
		return "", -1, errors.New("internal error: key list is empty")
	}

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
		// Check if it's because *all* original keys are temporarily failing
		if len(km.failingKeys) > 0 && len(km.failingKeys) == int(numOriginalKeys) { // Compare with original count
			log.Println("All keys were temporarily failing. Reactivating all keys immediately.")

			// Collect indices to reactivate
			indicesToReactivate := make([]int, 0, len(km.failingKeys))
			for index := range km.failingKeys {
				indicesToReactivate = append(indicesToReactivate, index)
			}

			// Reactivate collected indices
			for _, index := range indicesToReactivate {
				if index >= 0 && index < int(numOriginalKeys) { // Ensure index is valid
					km.availableKeys[index] = km.keys[index]
					delete(km.failingKeys, index)
					// Optional: Log forceful reactivation
					// log.Printf("Forcefully reactivated key index %d", index)
				} else {
					log.Printf("Warning: Tried to reactivate key index %d which is out of bounds for the original key list.", index)
					delete(km.failingKeys, index) // Still remove from failing if somehow invalid
				}
			}
			// Do NOT return an error here. Proceed to step 3 to select a key.
			log.Printf("Reactivated %d keys. Now selecting next available key.", len(km.availableKeys))
		} else {
			// This case means no keys are available, and it's *not* because all are temporarily failing.
			log.Println("Error: No API keys currently available and not all keys were failing.")
			return "", -1, errors.New("no keys configured or available") // Return the original error
		}
	}

	// 3. Find the next available key using random start

	// Try finding an available key starting from a random index
	// We loop max len(km.keys) times to ensure we check every possible slot
	// in case the available keys are sparse.
	startIndex := rand.IntN(int(numOriginalKeys)) // Generate a random starting index
	for i := range int(numOriginalKeys) {
		currentIndex := (startIndex + i) % int(numOriginalKeys)
		keyIndex := currentIndex // Use the calculated index directly

		if key, ok := km.availableKeys[keyIndex]; ok {
			// Found an available key
			return key, keyIndex, nil
		}
	}

	// If we looped through all original indices and found nothing in availableKeys
	// (This implies availableKeys is empty, which should have been caught earlier or handled by the reactivation block)
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
