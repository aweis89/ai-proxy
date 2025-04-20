package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"reflect" // Ensure reflect is imported for helpers
	"strings"
	"testing"
	"time"
)

// assertNotNil checks if a value is nil.
// TODO: Move common assert helpers to a shared file.
func assertNotNil(t *testing.T, val interface{}, msgAndArgs ...interface{}) {
	t.Helper()
	if val == nil || (reflect.ValueOf(val).Kind() == reflect.Ptr && reflect.ValueOf(val).IsNil()) {
		message := "got nil, want non-nil value"
		if len(msgAndArgs) > 0 {
			baseMsg := msgAndArgs[0].(string)
			args := msgAndArgs[1:]
			message = fmt.Sprintf(baseMsg, args...) + "; " + message
		}
		t.Error(message)
	}
}

// --- Test createProxyModifyResponse ---

// Test that ModifyResponse marks keys as failed for non-retryable 4xx errors within the correct scope.
func TestCreateProxyModifyResponse_MarksKeyFailedOnNonRetryable4xx(t *testing.T) {
	keys := []string{"key1", "key2"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	modifier := createProxyModifyResponse(km)

	scope := "test.com|/v1/fail" // Example scope
	baseURL := "http://test.com/v1/fail"

	// Simulate key 0 was used for a 400 Bad Request
	ctx0 := context.WithValue(context.Background(), keyIndexContextKey, 0)
	req0 := httptest.NewRequest("POST", baseURL, nil).WithContext(ctx0)
	// Ensure the request URL host and path match the scope for accurate testing
	parsedURL0, _ := url.Parse(baseURL)
	// The context should hold the original host/path used for getNextKey/markKeyFailed
	// In a real scenario, resp.Request.URL might differ if the proxy modified it,
	// but ModifyResponse should build the scope based on the URL in resp.Request.
	// For this test, we ensure resp.Request.URL matches our intended scope.
	req0.URL = parsedURL0 // Set URL on the request that goes into the Response
	resp0 := &http.Response{
		StatusCode: http.StatusBadRequest, // 400
		Request:    req0,
		Body:       io.NopCloser(strings.NewReader("Bad input")),
	}
	err := modifier(resp0)
	assertNoError(t, err)

	// Check state for key 0 in the specific scope
	km.mu.Lock()
	state0 := getScopeState(t, km, scope) // Use helper to get state
	_, isAvailable0 := state0.availableKeys[0]
	_, isFailing0 := state0.failingKeys[0]
	km.mu.Unlock()

	if isAvailable0 {
		t.Errorf("Scope '%s': Expected key 0 to be removed from available keys for 400", scope)
	}
	if !isFailing0 {
		t.Errorf("Scope '%s': Expected key 0 to be added to failing keys for 400", scope)
	}

	// Simulate key 1 was used for a 403 Forbidden in the SAME scope
	ctx1 := context.WithValue(context.Background(), keyIndexContextKey, 1)
	req1 := httptest.NewRequest("GET", baseURL, nil).WithContext(ctx1)
	parsedURL1, _ := url.Parse(baseURL)
	req1.URL = parsedURL1
	resp1 := &http.Response{
		StatusCode: http.StatusForbidden, // 403
		Request:    req1,
		Body:       io.NopCloser(strings.NewReader("Access denied")),
	}
	err = modifier(resp1)
	assertNoError(t, err)

	// Check state for key 1 in the specific scope
	km.mu.Lock()
	state1 := getScopeState(t, km, scope) // Get state again
	_, isAvailable1 := state1.availableKeys[1]
	_, isFailing1 := state1.failingKeys[1]
	km.mu.Unlock()

	if isAvailable1 {
		t.Errorf("Scope '%s': Expected key 1 to be removed from available keys for 403", scope)
	}
	if !isFailing1 {
		t.Errorf("Scope '%s': Expected key 1 to be added to failing keys for 403", scope)
	}

	// Check that both keys are now failing IN THIS SCOPE
	km.mu.Lock()
	finalState := getScopeState(t, km, scope)
	assertInt(t, len(finalState.availableKeys), 0)
	assertInt(t, len(finalState.failingKeys), 2)
	km.mu.Unlock()

	// Check another scope remains unaffected
	otherScope := "unaffected.com|/v1/ok"
	_, _, errOther := km.getNextKey(otherScope) // Access to create/check
	assertNoError(t, errOther)
	km.mu.Lock()
	otherState := getScopeState(t, km, otherScope)
	assertInt(t, len(otherState.availableKeys), 2) // Both keys should be available
	assertInt(t, len(otherState.failingKeys), 0)
	km.mu.Unlock()

	// Ensure response bodies are still readable after being logged
	bodyBytes0, readErr0 := io.ReadAll(resp0.Body)
	assertNoError(t, readErr0)
	assertString(t, string(bodyBytes0), "Bad input")

	bodyBytes1, readErr1 := io.ReadAll(resp1.Body)
	assertNoError(t, readErr1)
	assertString(t, string(bodyBytes1), "Access denied")
}

// Test that ModifyResponse does NOT mark keys as failed for 2xx, 5xx, or 429 status codes.
func TestCreateProxyModifyResponse_DoesNotMarkKeyFailedOnSuccessOrRetryable(t *testing.T) {
	keys := []string{"key1"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	modifier := createProxyModifyResponse(km)
	scope := "test.com|/v1/ok" // Example scope
	baseURL := "http://test.com/v1/ok"

	// Test 200 OK
	ctx := context.WithValue(context.Background(), keyIndexContextKey, 0)
	req := httptest.NewRequest("GET", baseURL, nil).WithContext(ctx)
	parsedURL, _ := url.Parse(baseURL)
	// Set the URL on the request that will be embedded in the Response object
	req.URL = parsedURL
	resp := &http.Response{
		StatusCode: http.StatusOK, // 200
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("Success")),
	}

	err := modifier(resp)
	assertNoError(t, err)

	// Check key 0 is still available (need to check scope state)
	// Get the scope state to check.
	// Note: Calling modifier doesn't create the scope if it doesn't exist,
	// because getNextKey is called elsewhere (in transport).
	// In this test, the scope *shouldn't* exist yet as we haven't called getNextKey
	// for this specific scope before calling modifier(resp) for the 200 OK.
	km.mu.Lock()
	state, scopeExists := km.scopes[scope]
	km.mu.Unlock()

	if scopeExists && state != nil {
		km.mu.Lock() // Lock again to access state fields safely
		_, isAvailable := state.availableKeys[0]
		_, isFailing := state.failingKeys[0]
		km.mu.Unlock()
		// If the scope somehow exists (it shouldn't at this point), the key should still be available and not failing
		if !isAvailable {
			t.Errorf("Scope '%s': Expected key 0 to still be available after 200", scope)
		}
		if isFailing {
			t.Errorf("Scope '%s': Expected key 0 not to be failing after 200", scope)
		}
	} // If scope doesn't exist, that's the expected state, as modifier doesn't create scopes.

	// Ensure response body is still readable
	bodyBytes, readErr := io.ReadAll(resp.Body)
	assertNoError(t, readErr)
	assertString(t, string(bodyBytes), "Success")
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes)) // Restore body for subsequent reads

	// --- Test 500 Internal Server Error ---
	// Need to ensure the scope exists before checking its state after the 500 response
	// because the checks below assume the scope state exists.
	_, _, err = km.getNextKey(scope) // This call creates the scope state if it doesn't exist
	assertNoError(t, err)

	ctx500 := context.WithValue(context.Background(), keyIndexContextKey, 0)
	req500 := httptest.NewRequest("GET", baseURL, nil).WithContext(ctx500) // Use baseURL here
	req500.URL = parsedURL
	resp500 := &http.Response{
		StatusCode: http.StatusInternalServerError, // 500
		Request:    req500,
		Body:       io.NopCloser(strings.NewReader("Server Error")),
	}
	err = modifier(resp500)
	assertNoError(t, err)
	km.mu.Lock()
	state500 := getScopeState(t, km, scope) // Now we can safely get the state
	_, isAvailable500 := state500.availableKeys[0]
	_, isFailing500 := state500.failingKeys[0]
	km.mu.Unlock()
	if !isAvailable500 {
		t.Errorf("Scope '%s': Expected key 0 to still be available after 500", scope)
	}
	if isFailing500 {
		t.Errorf("Scope '%s': Expected key 0 not to be failing after 500", scope)
	}
	bodyBytes500, readErr500 := io.ReadAll(resp500.Body) // Read body after checks
	assertNoError(t, readErr500)
	assertString(t, string(bodyBytes500), "Server Error")
	resp500.Body = io.NopCloser(bytes.NewReader(bodyBytes500)) // Restore body

	// --- Test 429 Too Many Requests ---
	ctx429 := context.WithValue(context.Background(), keyIndexContextKey, 0)
	req429 := httptest.NewRequest("GET", baseURL, nil).WithContext(ctx429) // Use baseURL here
	req429.URL = parsedURL
	resp429 := &http.Response{
		StatusCode: http.StatusTooManyRequests, // 429
		Request:    req429,
		Body:       io.NopCloser(strings.NewReader("Rate Limited")),
	}
	err = modifier(resp429)
	assertNoError(t, err)
	km.mu.Lock()
	state429 := getScopeState(t, km, scope) // Scope should exist from 500 test setup
	_, isAvailable429 := state429.availableKeys[0]
	_, isFailing429 := state429.failingKeys[0]
	km.mu.Unlock()
	if !isAvailable429 {
		t.Errorf("Scope '%s': Expected key 0 to still be available after 429 (handled by transport)", scope)
	}
	if isFailing429 {
		t.Errorf("Scope '%s': Expected key 0 not to be failing after 429 (handled by transport)", scope)
	}
	bodyBytes429, readErr429 := io.ReadAll(resp429.Body) // Read body after checks
	assertNoError(t, readErr429)
	assertString(t, string(bodyBytes429), "Rate Limited")
	// No need to restore body for the last check in the test
}

// Test resilience when key index is missing from context
func TestCreateProxyModifyResponse_HandlesMissingKeyIndex(t *testing.T) {
	keys := []string{"key1"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	modifier := createProxyModifyResponse(km)
	scope := "test.com|/v1/mising" // Example scope
	baseURL := "http://test.com/v1/mising"

	req := httptest.NewRequest("GET", baseURL, nil) // No key index in context
	parsedURL, _ := url.Parse(baseURL)
	// Set URL on the request that goes into the Response
	req.URL = parsedURL
	resp := &http.Response{
		StatusCode: http.StatusNotFound, // 404 (use a code that *would* trigger marking)
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("Error")),
	}

	// Capture log output
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr) // Restore default logger

	err := modifier(resp)
	assertNoError(t, err) // Should not return an error itself

	// Check key 0 was NOT marked as failed (scope state shouldn't even exist unless created elsewhere)
	km.mu.Lock()
	_, scopeExists := km.scopes[scope]
	km.mu.Unlock()
	if scopeExists {
		// If the scope exists, check the key wasn't marked failed
		km.mu.Lock()
		state := getScopeState(t, km, scope)
		_, isFailing := state.failingKeys[0]
		km.mu.Unlock()
		if isFailing {
			t.Errorf("Scope '%s': Expected key 0 not to be failing when index missing from context", scope)
		}
	} // else: scope not existing is also correct

	// Check log output for warning
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "Warning: No key index found in request context") {
		t.Errorf("Expected log warning about missing key index, got: %s", logOutput)
	}
	// Check that the non-2xx logging happened without key index info
	if !strings.Contains(logOutput, "Received non-2xx status: 404 (Key Index Unknown, Scope Unknown)") {
		t.Errorf("Expected log message about non-2xx status without key index, got: %s", logOutput)
	}
}

// --- Test createProxyErrorHandler ---

// Test the error handler when a generic error is passed
func TestCreateProxyErrorHandler_HandlesGenericError(t *testing.T) {
	handler := createProxyErrorHandler()
	scope := "testerror.com|/v1/err"
	baseURL := "http://testerror.com/v1/err"
	req := httptest.NewRequest("GET", baseURL, nil)
	parsedURL, _ := url.Parse(baseURL)
	req.URL = parsedURL
	rr := httptest.NewRecorder()
	genericErr := errors.New("connection refused")

	// Add a key index to context for logging verification
	ctx := context.WithValue(context.Background(), keyIndexContextKey, 5)
	req = req.WithContext(ctx)

	// Capture log output
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	handler(rr, req, genericErr)

	resp := rr.Result()
	body, _ := io.ReadAll(resp.Body)

	assertInt(t, resp.StatusCode, http.StatusBadGateway)
	assertString(t, strings.TrimSpace(string(body)), "Proxy Error: Upstream server failed after retries")

	// Check log output includes the generic error, key index, and scope
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "Proxy ErrorHandler triggered after transport/retries: connection refused") {
		t.Errorf("Expected log message indicating handler trigger and error, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, fmt.Sprintf("-> Scope '%s': Last attempt used key index 5", scope)) {
		t.Errorf("Expected log message indicating scope and last key index used, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, fmt.Sprintf("--> Scope '%s': Responding to client with status: 502", scope)) {
		t.Errorf("Expected log message indicating scope and response status 502, got: %s", logOutput)
	}
}

// Test the error handler when the error includes status code (proxyErrorWithStatus)
func TestCreateProxyErrorHandler_HandlesProxyErrorWithStatus(t *testing.T) {
	handler := createProxyErrorHandler()
	scope := "testerror.com|/v1/statuserr"
	baseURL := "http://testerror.com/v1/statuserr"
	req := httptest.NewRequest("GET", baseURL, nil)
	parsedURL, _ := url.Parse(baseURL)
	req.URL = parsedURL
	rr := httptest.NewRecorder()
	statusErr := &proxyErrorWithStatus{
		error:      errors.New("upstream unavailable"),
		StatusCode: http.StatusServiceUnavailable, // 503
	}

	// Capture log output
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	handler(rr, req, statusErr)

	resp := rr.Result()
	body, _ := io.ReadAll(resp.Body)

	assertInt(t, resp.StatusCode, http.StatusServiceUnavailable) // Should use status from error
	assertString(t, strings.TrimSpace(string(body)), "upstream unavailable")

	// Check log output
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "Proxy ErrorHandler triggered after transport/retries: upstream unavailable") {
		t.Errorf("Expected log message indicating handler trigger and error, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, fmt.Sprintf("--> Scope '%s': Responding to client with upstream status: 503", scope)) {
		t.Errorf("Expected log message indicating scope and response status 503, got: %s", logOutput)
	}
}

// Test the error handler when the error is context.Canceled
func TestCreateProxyErrorHandler_HandlesContextCanceled(t *testing.T) {
	handler := createProxyErrorHandler()
	scope := "testerror.com|/v1/cancel"
	baseURL := "http://testerror.com/v1/cancel"
	req := httptest.NewRequest("GET", baseURL, nil)
	parsedURL, _ := url.Parse(baseURL)
	req.URL = parsedURL
	rr := httptest.NewRecorder()
	cancelErr := context.Canceled

	// Capture log output
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	handler(rr, req, cancelErr)

	resp := rr.Result()
	body, _ := io.ReadAll(resp.Body)

	assertInt(t, resp.StatusCode, http.StatusRequestTimeout) // 408
	assertString(t, strings.TrimSpace(string(body)), "Client connection closed")

	// Check log output
	logOutput := logBuf.String()
	expectedTriggerMsg := fmt.Sprintf("Proxy ErrorHandler triggered after transport/retries: %v", cancelErr)
	if !strings.Contains(logOutput, expectedTriggerMsg) {
		t.Errorf("Expected log message indicating handler trigger and cancel error, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, fmt.Sprintf("-> Scope '%s': Key index for last attempt not found", scope)) {
		t.Errorf("Expected log message about scope and missing key index for canceled context, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, fmt.Sprintf("--> Scope '%s': Responding to client with status: %d", scope, http.StatusRequestTimeout)) {
		t.Errorf("Expected log message indicating scope and response status %d, got: %s", http.StatusRequestTimeout, logOutput)
	}
}

// --- Test createMainHandler (Basic Tests) ---

// Helper to create a minimal proxy for handler tests, including the retryTransport.
func newTestProxy(targetServer *httptest.Server, keyMan *keyManager, keyParam string, headerAuthPaths []string) *httputil.ReverseProxy {
	targetURL, _ := url.Parse(targetServer.URL)
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Setup transport
	retryTransport := newRetryTransport(http.DefaultTransport, keyMan, keyParam, headerAuthPaths)
	proxy.Transport = retryTransport

	// Setup simplified director
	originalDirector := proxy.Director
	proxy.Director = createProxyDirector(targetURL, originalDirector)

	// Setup other handlers
	proxy.ModifyResponse = createProxyModifyResponse(keyMan)
	proxy.ErrorHandler = createProxyErrorHandler()
	return proxy
}

func TestCreateMainHandler_CorsHeaders(t *testing.T) {
	// Setup a dummy target server that checks the key
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") == "" && r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, "Missing key")
			return
		}
		fmt.Fprintln(w, "Hello from target")
	}))
	defer targetServer.Close()

	keys := []string{"testkey"}
	km, _ := newKeyManager(keys, 1*time.Minute)
	keyParam := "key"
	headerPaths := []string{"/openai/"} // Example header paths
	proxy := newTestProxy(targetServer, km, keyParam, headerPaths)
	mainHandler := createMainHandler(proxy, false, "") // addGoogleSearch=false

	// Test GET request (retryTransport should add key to query param)
	reqGet := httptest.NewRequest("GET", "http://localhost:8080/some/path", nil)
	rrGet := httptest.NewRecorder()
	mainHandler(rrGet, reqGet)

	respGet := rrGet.Result()
	assertString(t, respGet.Header.Get("Access-Control-Allow-Origin"), "*")
	assertString(t, respGet.Header.Get("Access-Control-Allow-Methods"), "GET, POST, PUT, DELETE, OPTIONS, PATCH")
	assertString(t, respGet.Header.Get("Access-Control-Allow-Headers"), "Content-Type, Authorization, X-Requested-With")
	assertInt(t, respGet.StatusCode, http.StatusOK)

	// Test OPTIONS request
	reqOptions := httptest.NewRequest("OPTIONS", "http://localhost:8080/some/path", nil)
	reqOptions.Header.Set("Access-Control-Request-Method", "POST")
	reqOptions.Header.Set("Access-Control-Request-Headers", "Content-Type")
	rrOptions := httptest.NewRecorder()
	mainHandler(rrOptions, reqOptions)

	respOptions := rrOptions.Result()
	assertString(t, respOptions.Header.Get("Access-Control-Allow-Origin"), "*")
	assertString(t, respOptions.Header.Get("Access-Control-Allow-Methods"), "GET, POST, PUT, DELETE, OPTIONS, PATCH")
	assertString(t, respOptions.Header.Get("Access-Control-Allow-Headers"), "Content-Type, Authorization, X-Requested-With")
	assertInt(t, respOptions.StatusCode, http.StatusOK)

	bodyOptions, _ := io.ReadAll(respOptions.Body)
	assertString(t, string(bodyOptions), "")
}

func TestCreateMainHandler_PostRequestForwarding(t *testing.T) {
	var receivedBody string
	var receivedApiKey string
	var receivedAuthHeader string
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		receivedBody = string(bodyBytes)
		// Check how the key was received by the target (set by retryTransport)
		receivedApiKey = r.URL.Query().Get("key")
		receivedAuthHeader = r.Header.Get("Authorization")
		if receivedApiKey == "" && receivedAuthHeader == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, "Missing key")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Target received POST")
	}))
	defer targetServer.Close()

	keys := []string{"postkey1"}
	km, _ := newKeyManager(keys, 1*time.Minute)
	keyParam := "key"
	headerPaths := []string{"/openai/"} // Path that should use header auth
	proxy := newTestProxy(targetServer, km, keyParam, headerPaths)
	mainHandler := createMainHandler(proxy, false, "") // addGoogleSearch=false

	postBody := `{"data": "value"}`

	// --- Test 1: Path NOT matching headerAuthPaths (should use query param) ---
	req1 := httptest.NewRequest("POST", "http://localhost:8080/non-openai/path", strings.NewReader(postBody))
	req1.Header.Set("Content-Type", "application/json")
	rr1 := httptest.NewRecorder()
	mainHandler(rr1, req1)

	resp1 := rr1.Result()
	assertInt(t, resp1.StatusCode, http.StatusOK)
	assertString(t, receivedBody, postBody)
	assertString(t, receivedApiKey, "postkey1")                   // Key should be in query param
	assertString(t, receivedAuthHeader, "")                       // Auth header should be empty
	receivedBody, receivedApiKey, receivedAuthHeader = "", "", "" // Reset

	// --- Test 2: Path matching headerAuthPaths (should use Authorization header) ---
	req2 := httptest.NewRequest("POST", "http://localhost:8080/openai/v1/complete", strings.NewReader(postBody))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	mainHandler(rr2, req2)

	resp2 := rr2.Result()
	assertInt(t, resp2.StatusCode, http.StatusOK)
	assertString(t, receivedBody, postBody)
	assertString(t, receivedApiKey, "")                    // Key should NOT be in query param
	assertString(t, receivedAuthHeader, "Bearer postkey1") // Key should be in header
}

// --- Test createMainHandler Body Modification ---

func TestCreateMainHandler_GeminiPathBodyModification(t *testing.T) {
	var receivedBody string
	var receivedApiKey string
	var receivedAuthHeader string // Declare here
	var receivedContentType string
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		receivedBody = string(bodyBytes)
		receivedApiKey = r.URL.Query().Get("key") // Gemini paths use query param by default
		receivedAuthHeader = r.Header.Get("Authorization")
		receivedContentType = r.Header.Get("Content-Type")
		if receivedApiKey == "" && receivedAuthHeader == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, "Missing key")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Target received POST")
	}))
	defer targetServer.Close()

	keys := []string{"geminikey"}
	km, _ := newKeyManager(keys, 1*time.Minute)
	keyParam := "key"
	headerPaths := []string{"/openai/"} // Gemini paths don't match this
	proxy := newTestProxy(targetServer, km, keyParam, headerPaths)
	// Enable google search addition
	mainHandler := createMainHandler(proxy, true, "") // addGoogleSearch=true

	// Test case 1: Simple JSON body, should have tools added
	postBody1 := `{"contents": [{"parts":[{"text":"hello"}]}]}`
	expectedBody1 := `{"contents":[{"parts":[{"text":"hello"}]}],"tools":[{"google_search":{}}]}`
	req1 := httptest.NewRequest("POST", "http://localhost:8080/v1beta/models/gemini-pro:generateContent", strings.NewReader(postBody1))
	req1.Header.Set("Content-Type", "application/json")
	rr1 := httptest.NewRecorder()
	mainHandler(rr1, req1)

	resp1 := rr1.Result()
	assertInt(t, resp1.StatusCode, http.StatusOK)
	assertString(t, receivedBody, expectedBody1) // Verify modified body
	assertString(t, receivedApiKey, "geminikey") // Verify key in query param
	assertString(t, receivedAuthHeader, "")      // Verify header is empty
	assertString(t, receivedContentType, "application/json")
	receivedBody, receivedApiKey, receivedAuthHeader, receivedContentType = "", "", "", "" // Reset

	// Test case 2: Body already contains tools array, trigger word found, should replace with google_search
	postBody2 := `{"contents": [{"parts":[{"text":"search now"}]}], "tools": [{"some_other_tool":{}}]}`
	expectedBody2 := `{"contents":[{"parts":[{"text":"search now"}]}],"tools":[{"google_search":{}}]}` // Replaced
	req2 := httptest.NewRequest("POST", "http://localhost:8080/v1beta/models/gemini-1.5-flash:generateContent", strings.NewReader(postBody2))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	searchHandler := createMainHandler(proxy, true, "search") // Add trigger word
	searchHandler(rr2, req2)

	resp2 := rr2.Result()
	assertInt(t, resp2.StatusCode, http.StatusOK)
	assertString(t, receivedBody, expectedBody2) // Verify modified body
	assertString(t, receivedApiKey, "geminikey") // Verify key in query param
	assertString(t, receivedAuthHeader, "")      // Verify header is empty
	assertString(t, receivedContentType, "application/json")
	receivedBody, receivedApiKey, receivedAuthHeader, receivedContentType = "", "", "", "" // Reset

	// Test case 3: Non-Gemini path, should NOT be modified
	mainHandlerNoModify := createMainHandler(proxy, true, "") // Still true, but path won't match
	postBody3 := `{"data": "value"}`
	req3 := httptest.NewRequest("POST", "http://localhost:8080/other/api/v1/generate", strings.NewReader(postBody3))
	req3.Header.Set("Content-Type", "application/json")
	rr3 := httptest.NewRecorder()
	mainHandlerNoModify(rr3, req3)

	resp3 := rr3.Result()
	assertInt(t, resp3.StatusCode, http.StatusOK)
	assertString(t, receivedBody, postBody3)     // Verify original body
	assertString(t, receivedApiKey, "geminikey") // Verify key in query param
	assertString(t, receivedAuthHeader, "")      // Verify header is empty
	assertString(t, receivedContentType, "application/json")
}

func TestCreateMainHandler_AddGoogleSearchFalse(t *testing.T) {
	// Verify body is NOT modified when addGoogleSearch is false, even for Gemini paths
	var receivedBody string
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		receivedBody = string(bodyBytes)
		if r.URL.Query().Get("key") == "" && r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer targetServer.Close()

	keys := []string{"nokey"}
	km, _ := newKeyManager(keys, 1*time.Minute)
	keyParam := "key"
	headerPaths := []string{"/openai/"} // Example header paths
	proxy := newTestProxy(targetServer, km, keyParam, headerPaths)
	mainHandler := createMainHandler(proxy, false, "") // addGoogleSearch=false

	postBody := `{"contents": [{"parts":[{"text":"hello"}]}]}`
	// Path matches Gemini pattern but not header path, should use query param
	req := httptest.NewRequest("POST", "http://localhost:8080/v1beta/models/gemini-pro:generateContent", strings.NewReader(postBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mainHandler(rr, req)

	resp := rr.Result()
	assertInt(t, resp.StatusCode, http.StatusOK)
	assertString(t, receivedBody, postBody) // Body should be unmodified
}