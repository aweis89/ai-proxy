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

// --- Test createProxyDirector ---

func TestCreateProxyDirector_AddsKeyToQueryAndRotates(t *testing.T) {
	keys := []string{"key1", "key2", "key3"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	targetURL, _ := url.Parse("http://example.com")
	keyParam := "api_key"
	originalDirector := func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		// Path changes per request in loop
	}
	// Test with paths that should use query params (not matching header paths)
	headerPaths := []string{"/openai/", "/v1/special"}
	director := createProxyDirector(km, targetURL, keyParam, headerPaths, originalDirector)

	usedIndices := make(map[int]bool)
	numRequests := len(keys) * 3 // Make enough requests to likely see rotation

	for i := 0; i < numRequests; i++ {
		path := fmt.Sprintf("/test%d", i)
		req := httptest.NewRequest("GET", path, nil)
		// Manually set path in director for test request
		originalDirector(req)
		req.URL.Path = path

		director(req)

		// Check key is added to query param
		usedKey := req.URL.Query().Get(keyParam)
		authHeader := req.Header.Get("Authorization")
		keyIndexVal := req.Context().Value(keyIndexContextKey)
		assertNotNil(t, keyIndexVal, "key index should be in context for request %d", i)
		keyIndex := keyIndexVal.(int)

		if keyIndex < 0 || keyIndex >= len(keys) {
			t.Fatalf("Request %d: Invalid key index %d returned", i, keyIndex)
		}
		if usedKey != keys[keyIndex] {
			t.Errorf("Request %d: Expected key %q in query param %q, got %q for index %d", i, keys[keyIndex], keyParam, usedKey, keyIndex)
		}
		if authHeader != "" {
			t.Errorf("Request %d: Expected Authorization header to be empty, got %q", i, authHeader)
		}
		assertString(t, req.URL.Scheme, "http")
		assertString(t, req.URL.Host, "example.com")
		assertString(t, req.Host, "example.com")

		usedIndices[keyIndex] = true
	}

	// Check if multiple keys were used (highly likely with len*3 requests)
	if len(keys) > 1 && len(usedIndices) < 2 {
		t.Errorf("Expected at least 2 different key indices to be used over %d requests, only got %d unique indices (%v)", numRequests, len(usedIndices), usedIndices)
	}
}

func TestCreateProxyDirector_UsesAuthorizationHeader(t *testing.T) {
	keys := []string{"auth_key1"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	targetURL, _ := url.Parse("http://example.com")
	keyParam := "api_key" // Should not be used
	testPath := "/openai/v1/chat/completions" // Use a path that triggers header auth
	originalDirector := func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = testPath // Set the correct path
	}
	// Ensure the test path matches one of the configured header paths
	headerPaths := []string{"/openai/", "/v1/special"}
	director := createProxyDirector(km, targetURL, keyParam, headerPaths, originalDirector)

	req := httptest.NewRequest("GET", testPath, nil) // Use the correct path (/openai/v1/chat/completions)
	req.Header.Set("Authorization", "Bearer old_token") // Set an existing header
	director(req)

	// Check Authorization header is updated
	authHeader := req.Header.Get("Authorization")
	assertString(t, authHeader, "Bearer auth_key1")

	// Check query parameter is NOT set
	queryKey := req.URL.Query().Get(keyParam)
	assertString(t, queryKey, "")

	keyIndexVal := req.Context().Value(keyIndexContextKey)
	assertNotNil(t, keyIndexVal, "key index should be in context")
	assertInt(t, keyIndexVal.(int), 0)
	assertString(t, req.Host, "example.com")
}

func TestCreateProxyDirector_NoKeysAvailable(t *testing.T) {
	// Use a key manager deliberately configured to have no keys initially
	// (Though newKeyManager prevents this, we test the director's handling)
	km := &keyManager{
		keys:            []string{},
		availableKeys:   map[int]string{},
		failingKeys:     map[int]time.Time{},
		removalDuration: 1 * time.Minute,
	}
	targetURL, _ := url.Parse("http://example.com")
	keyParam := "api_key"
	originalDirector := func(req *http.Request) {} // Simple original director
	headerPaths := []string{"/openai/"} // Doesn't matter for this test
	director := createProxyDirector(km, targetURL, keyParam, headerPaths, originalDirector)

	req := httptest.NewRequest("GET", "/test", nil) // Path doesn't matter here
	director(req)

	// Check if error is added to context
	errVal := req.Context().Value(proxyErrorContextKey)
	assertNotNil(t, errVal, "Error should be in context when no keys are available")
	err, ok := errVal.(error)
	if !ok {
		t.Fatalf("Expected error in context, got %T", errVal)
	}
	// The specific error comes from keyManager.getNextKey
	assertError(t, err, errors.New("internal error: key list is empty"))

	// Check that key was NOT added
	queryKey := req.URL.Query().Get(keyParam)
	assertString(t, queryKey, "")
	authHeader := req.Header.Get("Authorization")
	assertString(t, authHeader, "")
}

// --- Test createProxyModifyResponse ---

func TestCreateProxyModifyResponse_MarksKeyFailedOn4xx(t *testing.T) {
	keys := []string{"key1", "key2"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	modifier := createProxyModifyResponse(km)

	// Simulate key 0 was used for this request
	ctx := context.WithValue(context.Background(), keyIndexContextKey, 0)
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests, // 429
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("Error details")), // Add a body
	}

	err := modifier(resp)
	assertNoError(t, err)

	// Check if key 0 is now failing
	km.mu.Lock()
	_, isAvailable := km.availableKeys[0]
	_, isFailing := km.failingKeys[0]
	km.mu.Unlock()

	if isAvailable {
		t.Error("Expected key 0 to be removed from available keys, but it was found")
	}
	if !isFailing {
		t.Error("Expected key 0 to be added to failing keys, but it was not found")
	}

	// Simulate key 1 was used for a 400 Bad Request
	ctx1 := context.WithValue(context.Background(), keyIndexContextKey, 1)
	req1 := httptest.NewRequest("POST", "/", nil).WithContext(ctx1)
	resp1 := &http.Response{
		StatusCode: http.StatusBadRequest, // 400
		Request:    req1,
		Body:       io.NopCloser(strings.NewReader("Bad input")),
	}
	err = modifier(resp1)
	assertNoError(t, err)

	km.mu.Lock()
	_, isAvailable1 := km.availableKeys[1]
	_, isFailing1 := km.failingKeys[1]
	km.mu.Unlock()

	if isAvailable1 {
		t.Error("Expected key 1 to be removed from available keys")
	}
	if !isFailing1 {
		t.Error("Expected key 1 to be added to failing keys")
	}

	// Check that both keys are now failing
	km.mu.Lock()
	assertInt(t, len(km.availableKeys), 0)
	assertInt(t, len(km.failingKeys), 2)
	km.mu.Unlock()

	// Ensure response body is still readable after being logged (important!)
	bodyBytes, readErr := io.ReadAll(resp.Body)
	assertNoError(t, readErr)
	assertString(t, string(bodyBytes), "Error details")

	bodyBytes1, readErr1 := io.ReadAll(resp1.Body)
	assertNoError(t, readErr1)
	assertString(t, string(bodyBytes1), "Bad input")
}

func TestCreateProxyModifyResponse_DoesNotMarkKeyFailedOn2xx(t *testing.T) {
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
}

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
	if !strings.Contains(logOutput, "Warning: No key index or proxy error found") {
		t.Errorf("Expected log warning about missing key index and proxy error, got: %s", logOutput)
	}
}

func TestCreateProxyModifyResponse_HandlesProxyErrorInContext(t *testing.T) {
	// Simulate a case where the director failed to get a key and put an error in context
	keys := []string{"key1"}
	km, _ := newKeyManager(keys, 5*time.Minute)
	modifier := createProxyModifyResponse(km)

	proxyErr := errors.New("director failed")
	// CRITICAL: Ensure only proxyErrorContextKey is set, not keyIndexContextKey,
	// to simulate the director failing *before* selecting a key.
	ctx := context.WithValue(context.Background(), proxyErrorContextKey, proxyErr)
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable, // Status code might reflect the proxy error
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("Proxy level error occurred")),
	}

	// Capture log output
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	err := modifier(resp)
	assertNoError(t, err) // Modifier itself shouldn't error

	// Check key 0 was NOT marked as failed, as no key index should have been involved
	km.mu.Lock()
	_, isAvailable := km.availableKeys[0]
	_, isFailing := km.failingKeys[0]
	km.mu.Unlock()

	if !isAvailable {
		t.Error("Expected key 0 to still be available when proxyError is in context and no keyIndex")
	}
	if isFailing {
		t.Error("Expected key 0 not to be failing when proxyError is in context and no keyIndex")
	}

	// Check log output for the proxy error message from the context
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "Error occurred during key selection for this request: director failed") {
		t.Errorf("Expected log message about proxyError from context, got: %s", logOutput)
	}
}

// --- Test createProxyErrorHandler ---

func TestCreateProxyErrorHandler_HandlesContextError(t *testing.T) {
	handler := createProxyErrorHandler()
	proxyErr := errors.New("no keys available")
	ctx := context.WithValue(context.Background(), proxyErrorContextKey, proxyErr)
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	// Capture log output
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	handler(rr, req, errors.New("some other transport error")) // Simulate a different error passed by proxy

	resp := rr.Result()
	body, _ := io.ReadAll(resp.Body)

	assertInt(t, resp.StatusCode, http.StatusServiceUnavailable)
	assertString(t, strings.TrimSpace(string(body)), "Proxy error: "+proxyErr.Error())

	// Check log output includes the original transport error
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "Proxy ErrorHandler triggered: some other transport error") {
		t.Errorf("Expected log message with original transport error, got: %s", logOutput)
	}
}

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

	assertInt(t, resp.StatusCode, http.StatusBadGateway)
	assertString(t, strings.TrimSpace(string(body)), "Proxy Error: "+genericErr.Error())

	// Check log output includes the generic error and key index
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "Proxy ErrorHandler triggered: connection refused") {
		t.Errorf("Expected log message with generic error, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "Error occurred during request with key index 5: connection refused") {
		t.Errorf("Expected log message with key index info, got: %s", logOutput)
	}
}

// --- Test createMainHandler (Basic Tests) ---

// Helper to create a minimal proxy for handler tests
// Helper to create a minimal proxy for handler tests
// Allows specifying header paths for director configuration
func newTestProxy(targetServer *httptest.Server, keyMan *keyManager, headerAuthPaths []string) *httputil.ReverseProxy {
	targetURL, _ := url.Parse(targetServer.URL)
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	originalDirector := proxy.Director
	proxy.Director = createProxyDirector(keyMan, targetURL, "key", headerAuthPaths, originalDirector)
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
	headerPaths := []string{"/openai/"} // Example header paths
	proxy := newTestProxy(targetServer, km, headerPaths)
	mainHandler := createMainHandler(proxy, false, "") // addGoogleSearch=false

	// Test GET request (should use query param)
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
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		receivedBody = string(bodyBytes)
		receivedApiKey = r.URL.Query().Get("key") // Check key received by target
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Target received POST")
	}))
	defer targetServer.Close()

	keys := []string{"postkey1"}
	km, _ := newKeyManager(keys, 1*time.Minute)
	headerPaths := []string{"/openai/"} // Example header paths
	proxy := newTestProxy(targetServer, km, headerPaths)
	mainHandler := createMainHandler(proxy, false, "") // addGoogleSearch=false

	postBody := `{"data": "value"}`
	// Use a path that should use query param
	req := httptest.NewRequest("POST", "http://localhost:8080/non-gemini/path", strings.NewReader(postBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	mainHandler(rr, req)

	resp := rr.Result()
	assertInt(t, resp.StatusCode, http.StatusOK)
	assertString(t, receivedBody, postBody)
	assertString(t, receivedApiKey, "postkey1")
}

// --- Test createMainHandler Body Modification (Needs handlePostBody tests first) ---
// Add tests here later that specifically verify the body modification logic
// based on addGoogleSearch flag and path matching, using handlePostBody tests as a guide.

func TestCreateMainHandler_GeminiPathBodyModification(t *testing.T) {
	var receivedBody string
	var receivedApiKey string
	var receivedContentType string
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		receivedBody = string(bodyBytes)
		receivedApiKey = r.URL.Query().Get("key")
		receivedContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Target received POST")
	}))
	defer targetServer.Close()

	keys := []string{"geminikey"}
	km, _ := newKeyManager(keys, 1*time.Minute)
	headerPaths := []string{"/openai/"} // Example header paths
	proxy := newTestProxy(targetServer, km, headerPaths)
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
	assertString(t, receivedBody, expectedBody1)
	assertString(t, receivedApiKey, "geminikey")
	assertString(t, receivedContentType, "application/json")
	receivedBody = "" // Reset for next test
	receivedApiKey = ""
	receivedContentType = ""

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
	assertString(t, receivedBody, expectedBody2)
	assertString(t, receivedApiKey, "geminikey")
	assertString(t, receivedContentType, "application/json")
	receivedBody = "" // Reset
	receivedApiKey = ""
	receivedContentType = ""

	// Test case 3: Non-Gemini path, should NOT be modified
	mainHandlerNoModify := createMainHandler(proxy, true, "") // Still true, but path won't match
	postBody3 := `{"data": "value"}`
	req3 := httptest.NewRequest("POST", "http://localhost:8080/other/api/v1/generate", strings.NewReader(postBody3))
	req3.Header.Set("Content-Type", "application/json")
	rr3 := httptest.NewRecorder()
	mainHandlerNoModify(rr3, req3)

	resp3 := rr3.Result()
	assertInt(t, resp3.StatusCode, http.StatusOK)
	assertString(t, receivedBody, postBody3)
	assertString(t, receivedApiKey, "geminikey")
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
	headerPaths := []string{"/openai/"} // Example header paths
	proxy := newTestProxy(targetServer, km, headerPaths)
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
