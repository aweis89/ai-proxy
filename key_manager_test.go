package main

import (
	"testing"
	"time"
	"sync"
	"errors"
	"fmt"
	"strings"
	"math/rand/v2" // Use v2 consistently
)

// Helper function to assert errors
func assertError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		// Allow comparing error strings as a fallback if errors.Is doesn't match
		if got == nil || want == nil || got.Error() != want.Error() {
				t.Errorf("got error %q, want error %q", got, want)
		}
	}
}


// Helper function to assert no error
func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("got error %q, want no error", err)
	}
}

// Helper function to assert string equality
func assertString(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Helper function to assert int equality
func assertInt(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

// Helper function to assert map length
func assertMapLength(t *testing.T, m interface{}, want int) {
	t.Helper()
	var length int
	switch v := m.(type) {
	case map[int]string:
		length = len(v)
	case map[int]time.Time:
		length = len(v)
	default:
		t.Fatalf("unsupported map type for length check")
	}
	if length != want {
		t.Errorf("got map length %d, want %d", length, want)
	}
}


// --- Test NewKeyManager ---

func TestNewKeyManager_Success(t *testing.T) {
	keys := []string{"key1", "key2", "key3"}
	duration := 5 * time.Minute
	km, err := newKeyManager(keys, duration)

	assertNoError(t, err)
	if km == nil {
		t.Fatal("expected keyManager to be non-nil")
	}
	assertInt(t, len(km.keys), 3)
	assertMapLength(t, km.availableKeys, 3)
	assertMapLength(t, km.failingKeys, 0)
	assertInt(t, int(km.removalDuration), int(duration))
	assertString(t, km.availableKeys[0], "key1")
	assertString(t, km.availableKeys[1], "key2")
	assertString(t, km.availableKeys[2], "key3")
}

func TestNewKeyManager_NoKeys(t *testing.T) {
	keys := []string{}
	_, err := newKeyManager(keys, 5*time.Minute)
	assertError(t, err, errors.New("at least one API key must be provided"))
}

func TestNewKeyManager_EmptyKeys(t *testing.T) {
	keys := []string{"", "", ""}
	_, err := newKeyManager(keys, 5*time.Minute)
	// The log message is printed, but the error should be about no valid keys
	assertError(t, err, errors.New("no valid (non-empty) API keys found"))
}

func TestNewKeyManager_MixedEmptyKeys(t *testing.T) {
	keys := []string{"key1", "", "key3"}
	duration := 5 * time.Minute
	km, err := newKeyManager(keys, duration)

	assertNoError(t, err)
	if km == nil {
		t.Fatal("expected keyManager to be non-nil")
	}
	assertInt(t, len(km.keys), 3) // Original keys count remains 3
	assertMapLength(t, km.availableKeys, 2)
	assertString(t, km.availableKeys[0], "key1")
	_, ok := km.availableKeys[1]
	if ok {
		t.Error("expected index 1 (empty key) not to be in availableKeys")
	}
	assertString(t, km.availableKeys[2], "key3")
	assertMapLength(t, km.failingKeys, 0)
	assertInt(t, int(km.removalDuration), int(duration))

}

func TestNewKeyManager_ZeroDuration(t *testing.T) {
	keys := []string{"key1"}
	_, err := newKeyManager(keys, 0)
	assertError(t, err, errors.New("key removal duration must be positive"))
}

func TestNewKeyManager_NegativeDuration(t *testing.T) {
	keys := []string{"key1"}
	_, err := newKeyManager(keys, -5*time.Minute)
	assertError(t, err, errors.New("key removal duration must be positive"))
}


// --- Test GetNextKey ---

func TestGetNextKey_RoundRobin(t *testing.T) {
	keys := []string{"key1", "key2", "key3"}
	km, _ := newKeyManager(keys, 5*time.Minute)

	keyCounts := make(map[string]int)
	indexCounts := make(map[int]int)

	// Call getNextKey more times than the number of keys to see rotation
	for i := 0; i < len(keys)*2; i++ {
		key, index, err := km.getNextKey()
		assertNoError(t, err)
		if key == "" || index < 0 || index >= len(keys) {
			t.Fatalf("invalid key or index returned: key=%q, index=%d", key, index)
		}
		assertString(t, key, keys[index]) // Ensure key matches the key at the returned index
		keyCounts[key]++
		indexCounts[index]++
	}

	// Check if keys were rotated reasonably (exact distribution depends on random start)
	// Over a small number of calls (len*2), we expect *most* keys to be hit, but not necessarily all.
	// We mainly care that *some* rotation happens and it doesn't get stuck.
	if len(keyCounts) < 2 {
		t.Errorf("expected at least 2 unique keys after %d gets, got %d (%v)", len(keys)*2, len(keyCounts), keyCounts)
	}
	if len(indexCounts) < 2 {
		t.Errorf("expected at least 2 unique indices after %d gets, got %d (%v)", len(keys)*2, len(indexCounts), indexCounts)
	}
	// Check counts are reasonable (e.g., none hit all 6 times)
	for k, count := range keyCounts {
		if count <= 0 || count >= len(keys)*2 {
				t.Errorf("key %q count %d seems unreasonable for %d gets", k, count, len(keys)*2)
		}
	}
		for idx, count := range indexCounts {
		if count <= 0 || count >= len(keys)*2 {
				t.Errorf("index %d count %d seems unreasonable for %d gets", idx, count, len(keys)*2)
		}
	}
}

func TestGetNextKey_Reactivation(t *testing.T) {
	keys := []string{"key1", "key2"}
	shortDuration := 100 * time.Millisecond
	km, _ := newKeyManager(keys, shortDuration)

	// Get first key
	_, index1, err := km.getNextKey()
	assertNoError(t, err)

	// Mark it as failed
	km.markKeyFailed(index1)
	assertMapLength(t, km.availableKeys, 1)
	assertMapLength(t, km.failingKeys, 1)


	// Get the other key (should be the only one available)
	key2, index2, err := km.getNextKey()
	assertNoError(t, err)
	assertString(t, keys[index2], key2) // Make sure it's the other key
	if index1 == index2 {
		t.Errorf("expected different key index, got same index %d", index1)
	}

	// Mark the second key as failed
	km.markKeyFailed(index2)
	assertMapLength(t, km.availableKeys, 0)
	assertMapLength(t, km.failingKeys, 2)


	// Try getting a key now - should succeed after forced reactivation
	key3, index3, err := km.getNextKey()
	assertNoError(t, err) // Expect no error now
	if key3 == "" || index3 < 0 { // Check if a valid key was returned
		t.Fatalf("Expected a valid key after forced reactivation, got key=%q, index=%d", key3, index3)
	}
	assertMapLength(t, km.availableKeys, 2) // Both should be back
	assertMapLength(t, km.failingKeys, 0)


	// Wait for reactivation (original test logic, less relevant now but keep for structure)
	time.Sleep(shortDuration * 2) // Wait a bit longer than duration

	// Try getting a key again - should still succeed
	key4, index4, err := km.getNextKey()
	assertNoError(t, err)
	if key4 == "" || index4 < 0 {
		t.Errorf("expected a valid key after reactivation wait, got key=%q, index=%d", key4, index4)
	}
	assertMapLength(t, km.availableKeys, 2) // Still 2 available
	assertMapLength(t, km.failingKeys, 0)

	// Check if the returned key is one of the original ones
	found := false
	for _, k := range keys {
		if key3 == k {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("reactivated key %q is not one of the original keys %v", key3, keys)
	}
}

func TestGetNextKey_NoKeysAvailableInitially(t *testing.T) {
	// Simulate a state where initialization somehow resulted in no available keys
	// (Though newKeyManager prevents this, test the function's safeguard)
	km := &keyManager{
		keys:            []string{}, // No original keys
		availableKeys:   make(map[int]string),
		failingKeys:     make(map[int]time.Time),
		removalDuration: 5 * time.Minute,
	}

	_, _, err := km.getNextKey()
	// The error check needs to be more specific for this case
	assertError(t, err, errors.New("internal error: key list is empty")) // This is the actual error returned in this path
}


func TestGetNextKey_AllKeysFailing(t *testing.T) {
	keys := []string{"key1"}
	duration := 1 * time.Minute
	km, _ := newKeyManager(keys, duration)

	// Mark the only key as failed
	km.markKeyFailed(0)
	assertMapLength(t, km.availableKeys, 0)
	assertMapLength(t, km.failingKeys, 1)


	// Try to get a key - should succeed after forced reactivation
	key, index, err := km.getNextKey()
	assertNoError(t, err)
	if key == "" || index < 0 {
		t.Fatalf("Expected a valid key after forced reactivation, got key=%q, index=%d", key, index)
	}
	assertMapLength(t, km.availableKeys, 1) // Key should be back
	assertMapLength(t, km.failingKeys, 0)
}

// --- Test MarkKeyFailed ---

func TestMarkKeyFailed_MarkAvailableKey(t *testing.T) {
	keys := []string{"key1", "key2"}
	duration := 5 * time.Minute
	km, _ := newKeyManager(keys, duration)

	// Mark key at index 0
	km.markKeyFailed(0)

	km.mu.Lock() // Lock needed to safely check map state
	assertMapLength(t, km.availableKeys, 1)
	_, availableOk := km.availableKeys[0]
	if availableOk {
		t.Error("key 0 should not be in availableKeys after marking failed")
	}
	assertMapLength(t, km.failingKeys, 1)
	_, failingOk := km.failingKeys[0]
	if !failingOk {
		t.Error("key 0 should be in failingKeys after marking failed")
	}
	reactivationTime := km.failingKeys[0]
	km.mu.Unlock()

	// Check reactivation time is roughly correct (allow some delta for execution time)
	expectedReactivation := time.Now().Add(duration)
	if reactivationTime.Before(expectedReactivation.Add(-1*time.Second)) || reactivationTime.After(expectedReactivation.Add(1*time.Second)) {
		t.Errorf("expected reactivation time around %v, got %v", expectedReactivation, reactivationTime)
	}
}

func TestMarkKeyFailed_MarkAlreadyFailedKey(t *testing.T) {
	keys := []string{"key1"}
	duration := 5 * time.Minute
	km, _ := newKeyManager(keys, duration)

	// Mark key 0 as failed
	km.markKeyFailed(0)
	km.mu.Lock()
	initialReactivationTime := km.failingKeys[0]
	assertMapLength(t, km.availableKeys, 0)
	assertMapLength(t, km.failingKeys, 1)
	km.mu.Unlock()


	// Mark key 0 as failed *again*
	km.markKeyFailed(0) // This should be a no-op according to the log message

	km.mu.Lock()
	assertMapLength(t, km.availableKeys, 0) // Still 0 available
	assertMapLength(t, km.failingKeys, 1) // Still 1 failing
	// Ensure reactivation time didn't change
	assertInt(t, int(km.failingKeys[0].UnixNano()), int(initialReactivationTime.UnixNano()))
	km.mu.Unlock()
}

func TestMarkKeyFailed_MarkInvalidIndex(t *testing.T) {
	keys := []string{"key1"}
	duration := 5 * time.Minute
	km, _ := newKeyManager(keys, duration)

	// Mark an invalid index
	km.markKeyFailed(99) // Should be a no-op, logged

	km.mu.Lock()
	assertMapLength(t, km.availableKeys, 1) // Should still be 1 available
	assertMapLength(t, km.failingKeys, 0) // Should still be 0 failing
	km.mu.Unlock()
}


// --- Test Concurrency ---

func TestKeyManager_Concurrency(t *testing.T) {
	// Seed the random number generator for reproducibility in failure simulation if needed
	// rand.Seed(time.Now().UnixNano()) // Use fixed seed for deterministic testing if required

	keys := []string{"k1", "k2", "k3", "k4", "k5"}
	duration := 100 * time.Millisecond // Short duration for testing reactivation
	km, _ := newKeyManager(keys, duration)

	numGoroutines := 20
	numGetsPerRoutine := 50
	totalGets := numGoroutines * numGetsPerRoutine

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	keyUsage := sync.Map{} // Use sync.Map for concurrent access
	errorChan := make(chan error, numGoroutines) // Channel to collect errors from goroutines


	for i := 0; i < numGoroutines; i++ {
		go func(routineID int) {
			defer wg.Done()
			// For math/rand/v2, creating separate Rand instances is not the standard way
			// Use the global functions directly as they are concurrency-safe.

			for j := 0; j < numGetsPerRoutine; j++ {
				key, index, err := km.getNextKey()

				// If all keys happen to be failing temporarily, wait and retry
				if err != nil {
					if strings.Contains(err.Error(), "all keys are temporarily failing") {
						 sleepDuration := time.Duration(rand.IntN(50)+10) * time.Millisecond // Random small sleep using v2
						 // Avoid noisy logging in tests unless debugging: fmt.Printf("Routine %d: All keys failing, sleeping for %v\n", routineID, sleepDuration)
						 time.Sleep(sleepDuration)
						 j-- // Decrement j to retry this iteration
						 continue
					} else {
							 // Report unexpected error via channel
							 errorChan <- fmt.Errorf("Routine %d: Unexpected error from getNextKey: %w", routineID, err)
							 return // Stop this routine on unexpected error
					}
				}


				if key == "" || index < 0 || index >= len(keys) {
					errorChan <- fmt.Errorf("Routine %d: Invalid key/index: key=%q, index=%d", routineID, key, index)
					continue // Continue to next iteration, but report error
				}
				if key != km.keys[index] {
					errorChan <- fmt.Errorf("Routine %d: Mismatch between returned key %q and expected key %q at index %d", routineID, key, km.keys[index], index)
				}


				// Record key usage
				usageKey := fmt.Sprintf("%d-%s", index, key)
				count, _ := keyUsage.LoadOrStore(usageKey, 0)
				keyUsage.Store(usageKey, count.(int)+1)

				// Simulate some keys failing occasionally
				if rand.IntN(10) == 0 { // ~10% chance of failure using v2
					// Avoid noisy logging: fmt.Printf("Routine %d: Simulating failure for key index %d\n", routineID, index)
					km.markKeyFailed(index)
					// Don't immediately try to get another key after failure simulation
					// Let the reactivation logic work
					time.Sleep(5 * time.Millisecond)
				} else {
						// Simulate work
						time.Sleep(time.Duration(rand.IntN(5)+1) * time.Millisecond) // Use v2
				}
			}
		}(i)
	}

	wg.Wait()
	close(errorChan) // Close channel after all goroutines are done

	// Check for errors reported by goroutines
	for err := range errorChan {
		t.Error(err) // Report any errors found
	}


	// Basic check: Ensure the total number of successful gets is roughly correct
	totalRecordedGets := 0
	keyUsage.Range(func(key, value interface{}) bool {
		totalRecordedGets += value.(int)
		return true
	})

	fmt.Printf("Total gets attempted: %d, Total gets recorded: %d\n", totalGets, totalRecordedGets)
	// Allow for some discrepancy due to simulated failures and retries
	// If many routines hit the "all keys failing" state, the recorded count might be lower.
	// A very loose check:
	if totalRecordedGets < totalGets/3 {
		t.Errorf("Recorded gets (%d) significantly less than expected (~%d)", totalRecordedGets, totalGets)
	}


	// Final check: Wait for potential reactivations and ensure all keys become available eventually
	time.Sleep(duration * 2) // Wait longer than removal duration
	finalKey, finalIndex, finalErr := km.getNextKey() // Should succeed if keys reactivated
	assertNoError(t, finalErr)
	if finalKey == "" || finalIndex < 0 {
		t.Errorf("Expected a valid key after waiting for reactivation, but got key=%q, index=%d", finalKey, finalIndex)
	}
	km.mu.Lock()
	assertMapLength(t, km.availableKeys, len(keys)) // All keys should be available now
	assertMapLength(t, km.failingKeys, 0)        // No keys should be failing
	fmt.Printf("Final state after wait: Available=%d, Failing=%d\n", len(km.availableKeys), len(km.failingKeys))
	km.mu.Unlock()
}