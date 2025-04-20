package main

import (
	"errors"
	"fmt"
	"math/rand/v2" // Use v2 consistently
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Helper function to assert errors
func assertError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		// Allow comparing error strings as a fallback if errors.Is doesn't match
		// Handle potential nil errors gracefully
		gotStr := "<nil>"
		if got != nil {
			gotStr = got.Error()
		}
		wantStr := "<nil>"
		if want != nil {
			wantStr = want.Error()
		}
		if gotStr != wantStr {
			t.Errorf("got error %q, want error %q", gotStr, wantStr)
		}
	}
}

// Helper function to assert error contains specific string
func assertErrorContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Errorf("expected error containing %q, got nil", substr)
		return
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("expected error %q to contain %q", err.Error(), substr)
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

// Helper function to assert map length (using reflection for flexibility)
func assertMapLength(t *testing.T, m interface{}, want int) {
	t.Helper()
	// Use reflection for broader type support, though the specific maps are known here.
	// This replaces the type switch for brevity.
	value := reflect.ValueOf(m)
	if value.Kind() != reflect.Map {
		t.Fatalf("assertMapLength requires a map, got %T", m)
	}
	length := value.Len()
	if length != want {
		t.Errorf("got map length %d, want %d", length, want)
	}
}

// Helper to get scope state (requires km mutex to be held)
func getScopeState(t *testing.T, km *keyManager, scope string) *scopeState {
	t.Helper()
	// km.mu must be locked before calling this
	state, exists := km.scopes[scope]
	if !exists {
		// In most tests, we expect the scope to be created by getNextKey or markKeyFailed
		// If we need to explicitly test creation, use getOrCreateScopeState
		t.Fatalf("scope %q does not exist unexpectedly", scope)
	}
	return state
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
	assertInt(t, len(km.originalKeys), 3)
	assertInt(t, len(km.scopes), 0) // Scopes map starts empty
	assertInt(t, int(km.removalDuration), int(duration))

	// Force scope creation to check initial state
	km.mu.Lock()
	scopeState := km.getOrCreateScopeState("testScope")
	km.mu.Unlock()
	assertInt(t, len(scopeState.availableKeys), 3)
	assertInt(t, len(scopeState.failingKeys), 0)
	assertString(t, scopeState.availableKeys[0], "key1")
	assertString(t, scopeState.availableKeys[1], "key2")
	assertString(t, scopeState.availableKeys[2], "key3")
}

func TestNewKeyManager_NoKeys(t *testing.T) {
	keys := []string{}
	_, err := newKeyManager(keys, 5*time.Minute)
	assertErrorContains(t, err, "at least one API key must be provided")
}

func TestNewKeyManager_EmptyKeys(t *testing.T) {
	keys := []string{"", "", ""}
	_, err := newKeyManager(keys, 5*time.Minute)
	assertErrorContains(t, err, "no valid (non-empty) API keys found")
}

func TestNewKeyManager_MixedEmptyKeys(t *testing.T) {
	keys := []string{"key1", "", "key3"}
	duration := 5 * time.Minute
	km, err := newKeyManager(keys, duration)

	assertNoError(t, err)
	if km == nil {
		t.Fatal("expected keyManager to be non-nil")
	}
	assertInt(t, len(km.originalKeys), 3) // Original keys count remains 3
	assertInt(t, len(km.scopes), 0)       // Scopes map starts empty

	// Force scope creation to check initial state
	km.mu.Lock()
	scopeState := km.getOrCreateScopeState("testScope")
	km.mu.Unlock()

	assertInt(t, len(scopeState.availableKeys), 2)
	assertInt(t, len(scopeState.failingKeys), 0)
	assertString(t, scopeState.availableKeys[0], "key1")
	_, ok := scopeState.availableKeys[1] // Check if index 1 (empty key) exists
	if ok {
		t.Error("expected index 1 (empty key) not to be in availableKeys")
	}
	assertString(t, scopeState.availableKeys[2], "key3")
	assertInt(t, int(km.removalDuration), int(duration))
}

func TestNewKeyManager_ZeroDuration(t *testing.T) {
	keys := []string{"key1"}
	_, err := newKeyManager(keys, 0)
	assertErrorContains(t, err, "key removal duration must be positive")
}

func TestNewKeyManager_NegativeDuration(t *testing.T) {
	keys := []string{"key1"}
	_, err := newKeyManager(keys, -5*time.Minute)
	assertErrorContains(t, err, "key removal duration must be positive")
}

// --- Test GetNextKey ---

func TestGetNextKey_RoundRobinAndScopeCreation(t *testing.T) {
	keys := []string{"key1", "key2", "key3"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	scope := "scope1"

	keyCounts := make(map[string]int)
	indexCounts := make(map[int]int)

	// Call getNextKey more times than the number of keys to see rotation
	for i := 0; i < len(keys)*2; i++ {
		key, index, err := km.getNextKey(scope)
		assertNoError(t, err)
		if key == "" || index < 0 || index >= len(keys) {
			t.Fatalf("invalid key or index returned: key=%q, index=%d", key, index)
		}
		assertString(t, key, keys[index]) // Ensure key matches the key at the returned index
		keyCounts[key]++
		indexCounts[index]++
	}

	// Check if scope was created
	km.mu.Lock()
	_, scopeExists := km.scopes[scope]
	km.mu.Unlock()
	if !scopeExists {
		t.Errorf("scope %q was not created after getNextKey calls", scope)
	}

	// Check if keys were rotated reasonably (exact distribution depends on random start)
	if len(keyCounts) < 2 {
		t.Errorf("expected at least 2 unique keys after %d gets, got %d (%v)", len(keys)*2, len(keyCounts), keyCounts)
	}
	if len(indexCounts) < 2 {
		t.Errorf("expected at least 2 unique indices after %d gets, got %d (%v)", len(keys)*2, len(indexCounts), indexCounts)
	}
}

func TestGetNextKey_ReactivationWithinScope(t *testing.T) {
	keys := []string{"key1", "key2"}
	shortDuration := 50 * time.Millisecond // Shorter duration
	km, _ := newKeyManager(keys, shortDuration)
	scope := "reactivationScope"

	// --- Test setup ---
	// Get first key to ensure scope is created
	_, index1, err := km.getNextKey(scope)
	assertNoError(t, err)

	// Mark it as failed
	km.markKeyFailed(scope, index1)
	km.mu.Lock()
	state1 := getScopeState(t, km, scope)
	assertInt(t, len(state1.availableKeys), 1)
	assertInt(t, len(state1.failingKeys), 1)
	km.mu.Unlock()

	// Get the other key (should be the only one available)
	key2, index2, err := km.getNextKey(scope)
	assertNoError(t, err)
	assertString(t, keys[index2], key2) // Make sure it's the other key
	if index1 == index2 {
		t.Errorf("expected different key index, got same index %d", index1)
	}

	// Mark the second key as failed
	km.markKeyFailed(scope, index2)
	km.mu.Lock()
	state2 := getScopeState(t, km, scope)
	assertInt(t, len(state2.availableKeys), 0)
	assertInt(t, len(state2.failingKeys), 2)
	km.mu.Unlock()

	// --- Test reactivation ---
	// Try getting a key now - should fail as reactivation loop hasn't run
	_, _, err = km.getNextKey(scope)
	assertErrorContains(t, err, "all keys are temporarily rate limited or failing")

	// Wait for reactivation loop (plus a buffer)
	// The loop runs every minute by default, need km.reactivateKeys() for testing
	// Let's manually trigger reactivation for testing reliability.
	time.Sleep(shortDuration + 10*time.Millisecond) // Wait for keys to expire
	km.reactivateKeys()                             // Manually trigger check

	// Try getting a key again - should succeed now
	key3, index3, err := km.getNextKey(scope)
	assertNoError(t, err)
	if key3 == "" || index3 < 0 {
		t.Fatalf("Expected a valid key after reactivation, got key=%q, index=%d", key3, index3)
	}

	km.mu.Lock()
	state3 := getScopeState(t, km, scope)
	assertInt(t, len(state3.availableKeys), 2) // Both should be back
	assertInt(t, len(state3.failingKeys), 0)
	km.mu.Unlock()

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

func TestGetNextKey_NoKeysAvailableInScope(t *testing.T) {
	// Test when all keys are marked failing within a specific scope
	keys := []string{"key1"}
	duration := 1 * time.Minute
	km, _ := newKeyManager(keys, duration)
	scope := "allFailingScope"

	// Mark the only key as failed in this scope
	km.markKeyFailed(scope, 0)

	km.mu.Lock()
	state := getScopeState(t, km, scope)
	assertInt(t, len(state.availableKeys), 0)
	assertInt(t, len(state.failingKeys), 1)
	km.mu.Unlock()

	// Try to get a key - should fail until reactivation
	_, _, err := km.getNextKey(scope)
	assertErrorContains(t, err, "all keys are temporarily rate limited or failing")
}

func TestGetNextKey_ScopeIsolation(t *testing.T) {
	keys := []string{"key1", "key2"}
	duration := 1 * time.Minute
	km, _ := newKeyManager(keys, duration)
	scopeA := "scopeA"
	scopeB := "scopeB"

	// Mark key 0 as failed in scope A
	km.markKeyFailed(scopeA, 0)

	// Check scope A state
	km.mu.Lock()
	stateA := getScopeState(t, km, scopeA)
	assertInt(t, len(stateA.availableKeys), 1)
	assertInt(t, len(stateA.failingKeys), 1)
	_, key0AvailableA := stateA.availableKeys[0]
	if key0AvailableA {
		t.Error("Scope A: Key 0 should not be available")
	}
	_, key0FailingA := stateA.failingKeys[0]
	if !key0FailingA {
		t.Error("Scope A: Key 0 should be failing")
	}
	km.mu.Unlock()

	// Try to get key 0 in scope B - should succeed
	// Need to loop until we specifically get index 0 or give up
	foundKey0InB := false
	for i := 0; i < len(keys)*2; i++ { // Try a few times
		keyB, indexB, errB := km.getNextKey(scopeB)
		assertNoError(t, errB)
		if indexB == 0 {
			assertString(t, keyB, "key1")
			foundKey0InB = true
			break
		}
	}
	if !foundKey0InB {
		t.Errorf("Scope B: Failed to get key index 0 even though it should be available")
	}

	// Check scope B state (after potential creation)
	km.mu.Lock()
	stateB := getScopeState(t, km, scopeB)
	assertInt(t, len(stateB.availableKeys), 2) // Both keys should be available initially
	assertInt(t, len(stateB.failingKeys), 0)
	km.mu.Unlock()
}

// --- Test MarkKeyFailed ---

func TestMarkKeyFailed_MarkAvailableKeyInScope(t *testing.T) {
	keys := []string{"key1", "key2"}
	duration := 5 * time.Minute
	km, _ := newKeyManager(keys, duration)
	scope := "markScope"

	// Get a key first to ensure scope exists
	_, _, _ = km.getNextKey(scope)

	// Mark key at index 0
	km.markKeyFailed(scope, 0)

	km.mu.Lock()
	state := getScopeState(t, km, scope)
	assertInt(t, len(state.availableKeys), 1)
	_, availableOk := state.availableKeys[0]
	if availableOk {
		t.Error("key 0 should not be in availableKeys after marking failed")
	}
	assertInt(t, len(state.failingKeys), 1)
	_, failingOk := state.failingKeys[0]
	if !failingOk {
		t.Error("key 0 should be in failingKeys after marking failed")
	}
	reactivationTime := state.failingKeys[0]
	km.mu.Unlock()

	// Check reactivation time is roughly correct
	expectedReactivation := time.Now().Add(duration)
	if reactivationTime.Before(expectedReactivation.Add(-1*time.Second)) || reactivationTime.After(expectedReactivation.Add(1*time.Second)) {
		t.Errorf("expected reactivation time around %v, got %v", expectedReactivation, reactivationTime)
	}
}

func TestMarkKeyFailed_MarkAlreadyFailedKeyInScope(t *testing.T) {
	keys := []string{"key1"}
	duration := 5 * time.Minute
	km, _ := newKeyManager(keys, duration)
	scope := "doubleMarkScope"

	// Mark key 0 as failed
	km.markKeyFailed(scope, 0)
	km.mu.Lock()
	state1 := getScopeState(t, km, scope)
	initialReactivationTime := state1.failingKeys[0]
	assertInt(t, len(state1.availableKeys), 0)
	assertInt(t, len(state1.failingKeys), 1)
	km.mu.Unlock()

	// Mark key 0 as failed *again*
	km.markKeyFailed(scope, 0) // Should be a no-op

	km.mu.Lock()
	state2 := getScopeState(t, km, scope)
	assertInt(t, len(state2.availableKeys), 0) // Still 0 available
	assertInt(t, len(state2.failingKeys), 1)   // Still 1 failing
	// Ensure reactivation time didn't change
	assertInt(t, int(state2.failingKeys[0].UnixNano()), int(initialReactivationTime.UnixNano()))
	km.mu.Unlock()
}

func TestMarkKeyFailed_MarkInvalidIndexInScope(t *testing.T) {
	keys := []string{"key1"}
	duration := 5 * time.Minute
	km, _ := newKeyManager(keys, duration)
	scope := "invalidIndexScope"

	// Get key to create scope
	_, _, _ = km.getNextKey(scope)

	// Mark an invalid index
	km.markKeyFailed(scope, 99) // Should be a no-op, logged

	km.mu.Lock()
	state := getScopeState(t, km, scope)
	assertInt(t, len(state.availableKeys), 1) // Should still be 1 available
	assertInt(t, len(state.failingKeys), 0)   // Should still be 0 failing
	km.mu.Unlock()
}

// --- Test Reactivation Loop ---

func TestReactivationLoop(t *testing.T) {
	// This test relies on time passing, making it potentially flaky.
	// A manual trigger of reactivateKeys is generally preferred for unit tests.
	// Keeping this structure but with manual trigger.
	keys := []string{"k1", "k2"}
	shortDuration := 50 * time.Millisecond
	km, _ := newKeyManager(keys, shortDuration) // Background loop starts here

	scope1 := "loopScope1"
	scope2 := "loopScope2"

	// Mark k1 in scope1, k2 in scope2
	km.markKeyFailed(scope1, 0)
	km.markKeyFailed(scope2, 1)

	// Check initial state
	km.mu.Lock()
	s1 := getScopeState(t, km, scope1)
	s2 := getScopeState(t, km, scope2)
	assertInt(t, len(s1.availableKeys), 1)
	assertInt(t, len(s1.failingKeys), 1)
	assertInt(t, len(s2.availableKeys), 1)
	assertInt(t, len(s2.failingKeys), 1)
	km.mu.Unlock()

	// Wait for slightly longer than the duration
	time.Sleep(shortDuration + 20*time.Millisecond)

	// Manually trigger reactivation (instead of waiting for the loop's ticker)
	km.reactivateKeys()

	// Check state after reactivation
	km.mu.Lock()
	s1_after := getScopeState(t, km, scope1)
	s2_after := getScopeState(t, km, scope2)
	assertInt(t, len(s1_after.availableKeys), 2) // k1 should be back
	assertInt(t, len(s1_after.failingKeys), 0)
	assertInt(t, len(s2_after.availableKeys), 2) // k2 should be back
	assertInt(t, len(s2_after.failingKeys), 0)
	km.mu.Unlock()

	// Ensure we can get keys again
	_, _, err1 := km.getNextKey(scope1)
	assertNoError(t, err1)
	_, _, err2 := km.getNextKey(scope2)
	assertNoError(t, err2)
}

// --- Test Concurrency ---

func TestKeyManager_Concurrency(t *testing.T) {
	// Concurrency test needs significant rework for scopes.
	// Each goroutine could operate on a different scope or the same scope.

	t.Run("ConcurrentAccessSameScope", func(t *testing.T) {
		keys := []string{"k1", "k2", "k3", "k4", "k5"}
		duration := 50 * time.Millisecond // Short duration
		km, _ := newKeyManager(keys, duration)
		scope := "concurrentScope"

		numGoroutines := 20
		numGetsPerRoutine := 50

		var wg sync.WaitGroup
		wg.Add(numGoroutines)
		errorChan := make(chan error, numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(routineID int) {
				defer wg.Done()
				for j := 0; j < numGetsPerRoutine; j++ {
					key, index, err := km.getNextKey(scope)

					if err != nil {
						if strings.Contains(err.Error(), "all keys are temporarily rate limited or failing") {
							time.Sleep(time.Duration(rand.IntN(20)+5) * time.Millisecond)
							j-- // Retry
							continue
						} else {
							errorChan <- fmt.Errorf("Routine %d: Unexpected error: %w", routineID, err)
							return
						}
					}

					if key == "" || index < 0 || index >= len(keys) {
						errorChan <- fmt.Errorf("Routine %d: Invalid key/index: key=%q, index=%d", routineID, key, index)
						continue
					}
					if key != km.originalKeys[index] { // Check against originalKeys
						errorChan <- fmt.Errorf("Routine %d: Mismatch key=%q vs originalKeys[%d]=%q", routineID, key, index, km.originalKeys[index])
					}

					// Simulate some keys failing
					if rand.IntN(10) == 0 {
						km.markKeyFailed(scope, index)
						time.Sleep(5 * time.Millisecond)
					} else {
						time.Sleep(time.Duration(rand.IntN(5)+1) * time.Millisecond)
					}
				}
			}(i)
		}

		wg.Wait()
		close(errorChan)

		for err := range errorChan {
			t.Error(err)
		}

		// Final check: Wait and ensure keys reactivate
		time.Sleep(duration * 2)
		km.reactivateKeys() // Manual trigger
		km.mu.Lock()
		finalState := getScopeState(t, km, scope)
		assertInt(t, len(finalState.availableKeys), len(keys))
		assertInt(t, len(finalState.failingKeys), 0)
		km.mu.Unlock()
	})

	t.Run("ConcurrentAccessDifferentScopes", func(t *testing.T) {
		keys := []string{"k1", "k2", "k3"}
		duration := 50 * time.Millisecond
		km, _ := newKeyManager(keys, duration)

		numGoroutines := 15
		numGetsPerRoutine := 30

		var wg sync.WaitGroup
		wg.Add(numGoroutines)
		errorChan := make(chan error, numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(routineID int) {
				defer wg.Done()
				scope := fmt.Sprintf("scope-%d", routineID%5) // 5 different scopes
				for j := 0; j < numGetsPerRoutine; j++ {
					key, index, err := km.getNextKey(scope)

					if err != nil {
						// Less likely to hit all failing in different scopes, but handle
						if strings.Contains(err.Error(), "all keys are temporarily rate limited or failing") {
							time.Sleep(time.Duration(rand.IntN(20)+5) * time.Millisecond)
							j-- // Retry
							continue
						} else {
							errorChan <- fmt.Errorf("Routine %d (Scope %s): Unexpected error: %w", routineID, scope, err)
							return
						}
					}

					if key == "" || index < 0 || index >= len(keys) {
						errorChan <- fmt.Errorf("Routine %d (Scope %s): Invalid key/index: key=%q, index=%d", routineID, scope, key, index)
						continue
					}
					if key != km.originalKeys[index] {
						errorChan <- fmt.Errorf("Routine %d (Scope %s): Mismatch key=%q vs originalKeys[%d]=%q", routineID, scope, key, index, km.originalKeys[index])
					}

					// Simulate failure only in some scopes
					if routineID%5 == 0 && rand.IntN(5) == 0 { // Fail more often in scope-0
						km.markKeyFailed(scope, index)
						time.Sleep(5 * time.Millisecond)
					} else {
						time.Sleep(time.Duration(rand.IntN(5)+1) * time.Millisecond)
					}
				}
			}(i)
		}

		wg.Wait()
		close(errorChan)

		for err := range errorChan {
			t.Error(err)
		}

		// Final check: Wait and ensure keys reactivate across scopes
		time.Sleep(duration * 2)
		km.reactivateKeys() // Manual trigger

		km.mu.Lock()
		for i := 0; i < 5; i++ {
			scopeName := fmt.Sprintf("scope-%d", i)
			if finalState, exists := km.scopes[scopeName]; exists {
				assertInt(t, len(finalState.availableKeys), len(keys))
				assertInt(t, len(finalState.failingKeys), 0)
			} else {
				t.Errorf("Scope %s was expected to exist but didn't", scopeName)
			}
		}
		km.mu.Unlock()
	})
}
