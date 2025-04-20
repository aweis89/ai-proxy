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
	"strings"
	"testing"
	"time"
)

// assertNotNil checks if a value is nil.
// TODO: Move common assert helpers to a shared file.
func assertNotNil(t *testing.T, val interface{}, msgAndArgs ...interface{}) {
	t.Helper()
	if val == nil {
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

// Test that ModifyResponse marks keys as failed for non-retryable 4xx errors.
// Note: 429 is handled by the retryTransport, so we test other 4xx codes here.
func TestCreateProxyModifyResponse_MarksKeyFailedOnNonRetryable4xx(t *testing.T) {
	keys := []string{"key1", "key2"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	modifier := createProxyModifyResponse(km)

	// Simulate key 0 was used for a 400 Bad Request
	ctx0 := context.WithValue(context.Background(), keyIndexContextKey, 0)
	req0 := httptest.NewRequest("POST", "/", nil).WithContext(ctx0)
	resp0 := &http.Response{
		StatusCode: http.StatusBadRequest, // 400
		Request:    req0,
		Body:       io.NopCloser(strings.NewReader("Bad input")),
	}
	err := modifier(resp0)
	assertNoError(t, err)

	km.mu.Lock()
	_, isAvailable0 := km.availableKeys[0]
	_, isFailing0 := km.failingKeys[0]
	km.mu.Unlock()

	if isAvailable0 {
		t.Error("Expected key 0 to be removed from available keys for 400")
	}
	if !isFailing0 {
		t.Error("Expected key 0 to be added to failing keys for 400")
	}

	// Simulate key 1 was used for a 403 Forbidden
	ctx1 := context.WithValue(context.Background(), keyIndexContextKey, 1)
	req1 := httptest.NewRequest("GET", "/forbidden", nil).WithContext(ctx1)
	resp1 := &http.Response{
		StatusCode: http.StatusForbidden, // 403
		Request:    req1,
		Body:       io.NopCloser(strings.NewReader("Access denied")),
	}
	err = modifier(resp1)
	assertNoError(t, err)

	km.mu.Lock()
	_, isAvailable1 := km.availableKeys[1]
	_, isFailing1 := km.failingKeys[1]
	km.mu.Unlock()

	if isAvailable1 {
		t.Error("Expected key 1 to be removed from available keys for 403")
	}
	if !isFailing1 {
		t.Error("Expected key 1 to be added to failing keys for 403")
	}

	// Check that both keys are now failing
	km.mu.Lock()
	assertInt(t, len(km.availableKeys), 0)
	assertInt(t, len(km.failingKeys), 2)
	km.mu.Unlock()

	// Ensure response bodies are still readable after being logged
	bodyBytes0, readErr0 := io.ReadAll(resp0.Body)
	assertNoError(t, readErr0)
	assertString(t, string(bodyBytes0), "Bad input")

	bodyBytes1, readErr1 := io.ReadAll(resp1.Body)
	assertNoError(t, readErr1)
	assertString(t, string(bodyBytes1), "Access denied")
}

// Test that ModifyResponse does NOT mark keys as failed for 2xx or 5xx status codes.
// 5xx might be retried by the transport, and 2xx are successes.
// 429 is also handled by transport, so it shouldn't be marked here either.
func TestCreateProxyModifyResponse_DoesNotMarkKeyFailedOnSuccessOrRetryable(t *testing.T) {
	keys := []string{"key1"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	modifier := createProxyModifyResponse(km)

	ctx := context.WithValue(context.Background(), keyIndexContextKey, 0)
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	resp := &http.Response{
		StatusCode: http.StatusOK, // 200
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("Success")),
	}

	err := modifier(resp)
	assertNoError(t, err)

	// Check key 0 is still available
	km.mu.Lock()
	_, isAvailable := km.availableKeys[0]
	_, isFailing := km.failingKeys[0]
	km.mu.Unlock()

	if !isAvailable {
		t.Error("Expected key 0 to still be available")
	}
	if isFailing {
		t.Error("Expected key 0 not to be failing")
	}

	// Ensure response body is still readable
	bodyBytes, readErr := io.ReadAll(resp.Body)
	assertNoError(t, readErr)
	assertString(t, string(bodyBytes), "Success")

	// Test 500 Internal Server Error
	ctx500 := context.WithValue(context.Background(), keyIndexContextKey, 0)
	req500 := httptest.NewRequest("GET", "/", nil).WithContext(ctx500)
	resp500 := &http.Response{
		StatusCode: http.StatusInternalServerError, // 500
		Request:    req500,
		Body:       io.NopCloser(strings.NewReader("Server Error")),
	}
	err = modifier(resp500)
	assertNoError(t, err)
	km.mu.Lock()
	_, isAvailable500 := km.availableKeys[0]
	_, isFailing500 := km.failingKeys[0]
	km.mu.Unlock()
	if !isAvailable500 {
		t.Error("Expected key 0 to still be available after 500")
	}
	if isFailing500 {
		t.Error("Expected key 0 not to be failing after 500")
	}

	// Test 429 Too Many Requests (should be handled by transport, not marked here)
	ctx429 := context.WithValue(context.Background(), keyIndexContextKey, 0)
	req429 := httptest.NewRequest("GET", "/", nil).WithContext(ctx429)
	resp429 := &http.Response{
		StatusCode: http.StatusTooManyRequests, // 429
		Request:    req429,
		Body:       io.NopCloser(strings.NewReader("Rate Limited")),
	}
	err = modifier(resp429)
	assertNoError(t, err)
	km.mu.Lock()
	_, isAvailable429 := km.availableKeys[0]
	_, isFailing429 := km.failingKeys[0]
	km.mu.Unlock()
	if !isAvailable429 {
		t.Error("Expected key 0 to still be available after 429 (handled by transport)")
	}
	if isFailing429 {
		t.Error("Expected key 0 not to be failing after 429 (handled by transport)")
	}
}

// Test resilience when key index is missing from context (shouldn't happen normally with transport)
func TestCreateProxyModifyResponse_HandlesMissingKeyIndex(t *testing.T) {
	// This shouldn't happen in normal operation if director ran, but test resilience
	keys := []string{"key1"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	modifier := createProxyModifyResponse(km)

	req := httptest.NewRequest("GET", "/", nil) // No key index in context
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests, // 429
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("Error")),
	}

	// Capture log output
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr) // Restore default logger

	err := modifier(resp)
	assertNoError(t, err) // Should not return an error itself

	// Check key 0 was NOT marked as failed
	km.mu.Lock()
	_, isAvailable := km.availableKeys[0]
	_, isFailing := km.failingKeys[0]
	km.mu.Unlock()

	if !isAvailable {
		t.Error("Expected key 0 to still be available when index missing")
	}
	if isFailing {
		t.Error("Expected key 0 not to be failing when index missing")
	}

	// Check log output for warning
	logOutput := logBuf.String()
	// The specific check for "proxy error" is removed as director no longer sets it.
	if !strings.Contains(logOutput, "Warning: No key index found in request context") {
		t.Errorf("Expected log warning about missing key index, got: %s", logOutput)
	}
}

// --- Test createProxyErrorHandler ---

// Test the error handler when a generic error is passed (simulating transport failure after retries)
func TestCreateProxyErrorHandler_HandlesGenericError(t *testing.T) {
	handler := createProxyErrorHandler()
	req := httptest.NewRequest("GET", "/", nil) // No specific proxy error in context
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

	assertInt(t, resp.StatusCode, http.StatusBadGateway) // Should return 502
	assertString(t, strings.TrimSpace(string(body)), "Proxy Error: Upstream server failed after retries") // Check updated message

	// Check log output includes the generic error and key index
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "Proxy ErrorHandler triggered after transport/retries: connection refused") {
		t.Errorf("Expected log message indicating handler trigger and error, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "-> Last attempt used key index 5") {
		t.Errorf("Expected log message indicating last key index used, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "--> Responding to client with status: 502") {
		t.Errorf("Expected log message indicating response status 502, got: %s", logOutput)
	}
}

// Test the error handler when the error is context.Canceled
func TestCreateProxyErrorHandler_HandlesContextCanceled(t *testing.T) {
	handler := createProxyErrorHandler()
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	// Use context.DeadlineExceeded for a clearer timeout scenario, though Canceled is also common
	// Let's stick with Canceled as that's what the handler checks for explicitly.
	cancelErr := context.Canceled

	// Capture log output
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	handler(rr, req, cancelErr)

	resp := rr.Result()
	body, _ := io.ReadAll(resp.Body)

	// The handler uses http.StatusRequestTimeout (408) for context.Canceled.
	assertInt(t, resp.StatusCode, http.StatusRequestTimeout) // 408
	assertString(t, strings.TrimSpace(string(body)), "Client connection closed")

	// Check log output
	logOutput := logBuf.String()
	expectedTriggerMsg := fmt.Sprintf("Proxy ErrorHandler triggered after transport/retries: %v", cancelErr)
	if !strings.Contains(logOutput, expectedTriggerMsg) {
		t.Errorf("Expected log message indicating handler trigger and cancel error, got: %s", logOutput)
	}
	// Key index won't be present if context is canceled early
	if !strings.Contains(logOutput, "-> Key index for last attempt not found in context.") {
		t.Errorf("Expected log message about missing key index for canceled context, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, fmt.Sprintf("--> Responding to client with status: %d", http.StatusRequestTimeout)) {
		t.Errorf("Expected log message indicating response status %d, got: %s", http.StatusRequestTimeout, logOutput)
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
	proxy.Director = createProxyDirector(targetURL, originalDirector) // Simplified call

	// Setup other handlers
	proxy.ModifyResponse = createProxyModifyResponse(keyMan)
	proxy.ErrorHandler = createProxyErrorHandler()
	return proxy
}

func TestCreateMainHandler_CorsHeaders(t *testing.T) {
	// Setup a dummy target server
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	assertString(t, receivedApiKey, "postkey1") // Key should be in query param
	assertString(t, receivedAuthHeader, "")     // Auth header should be empty
	receivedBody, receivedApiKey, receivedAuthHeader = "", "", "" // Reset

	// --- Test 2: Path matching headerAuthPaths (should use Authorization header) ---
	req2 := httptest.NewRequest("POST", "http://localhost:8080/openai/v1/complete", strings.NewReader(postBody))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	mainHandler(rr2, req2)

	resp2 := rr2.Result()
	assertInt(t, resp2.StatusCode, http.StatusOK)
	assertString(t, receivedBody, postBody)
	assertString(t, receivedApiKey, "") // Key should NOT be in query param
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
	assertString(t, receivedAuthHeader, "")     // Verify header is empty
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
	assertString(t, receivedAuthHeader, "")     // Verify header is empty
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
	assertString(t, receivedAuthHeader, "")     // Verify header is empty
	assertString(t, receivedContentType, "application/json")
}

func TestCreateMainHandler_AddGoogleSearchFalse(t *testing.T) {
	// Verify body is NOT modified when addGoogleSearch is false, even for Gemini paths
	var receivedBody string
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		receivedBody = string(bodyBytes)
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
	assertString(t, receivedBody, postBody)
}
