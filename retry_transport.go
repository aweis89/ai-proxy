package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	// Added time import
)

// proxyErrorWithStatus wraps an error with the HTTP status code from the last response.
type proxyErrorWithStatus struct {
	error
	StatusCode int
}

const (
	maxRetries    = 3
	bodyReadLimit = 10 * 1024 * 1024 // Limit body size for buffering (e.g., 10MB)
)

// retryTransport handles API key injection, request modification based on path,
// and retries failed requests (e.g., on 429 errors or temporary network issues).
type retryTransport struct {
	underlyingTransport http.RoundTripper
	keyMan              *keyManager
	keyParam            string
	headerAuthPaths     []string
}

// newRetryTransport creates a new retryTransport.
func newRetryTransport(transport http.RoundTripper, km *keyManager, keyParam string, headerPaths []string) *retryTransport {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &retryTransport{
		underlyingTransport: transport,
		keyMan:              km,
		keyParam:            keyParam,
		headerAuthPaths:     headerPaths,
	}
}

// RoundTrip executes a single HTTP transaction, handling key selection,
// request modification, and retries.
func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var lastErr error
	var resp *http.Response
	var bodyBytes []byte
	var keyIndex int = -1 // Initialize keyIndex

	// --- Buffer request body if necessary ---
	// We need to buffer if it's not GET/HEAD/OPTIONS etc. *and* there's a body,
	// as we might need to send it multiple times on retry.
	if req.Body != nil && req.Body != http.NoBody && !isIdempotentMethod(req.Method) {
		var readErr error
		// Limit the amount read to prevent OOM errors with huge request bodies
		limitedReader := io.LimitReader(req.Body, bodyReadLimit)
		bodyBytes, readErr = io.ReadAll(limitedReader)
		req.Body.Close() // Close original body reader
		if readErr != nil {
			return nil, fmt.Errorf("failed to read request body for potential retry: %w", readErr)
		}
		// Check if the body was truncated
		if _, err := io.Copy(io.Discard, req.Body); err == nil {
			// If we could still read more from the original body, it means the limit was hit
			log.Printf("Warning: Request body exceeded %d bytes, potential truncation.", bodyReadLimit)
			// Decide if this should be a hard error or just a warning
			// return nil, fmt.Errorf("request body exceeded limit of %d bytes", bodyReadLimit)
		}
	}

	// --- Retry Loop ---
	for attempt := range maxRetries {
		// --- Create Scope Key ---
		// Use the original request's URL to build the scope key, as it doesn't change between retries.
		// Important: Use req.URL.Host and req.URL.Path from the *original* request passed to RoundTrip,
		// not from currentReq inside the loop, as currentReq might have its Host field modified by the director.
		scope := buildScopeKey(req.URL.Host, req.URL.Path)

		// --- Get API Key ---
		apiKey, currentKeyIndex, keyErr := rt.keyMan.getNextKey(scope)
		if keyErr != nil {
			log.Printf("[Retry Transport] Scope '%s': Error getting API key for attempt %d: %v", scope, attempt+1, keyErr)
			// If we couldn't get a key, even on the first attempt, return the error.
			if resp != nil {
				resp.Body.Close()
			}
			// Wrap the specific key error to give more context upstream
			return nil, &proxyErrorWithStatus{
				error:      fmt.Errorf("scope '%s': failed to get API key (attempt %d): %w", scope, attempt+1, keyErr),
				StatusCode: http.StatusServiceUnavailable, // Indicate no keys available for this scope
			}
		}
		keyIndex = currentKeyIndex // Store the index used for this attempt

		// --- Clone Request and Set Context/Body ---
		// Clone the request for this attempt to avoid modifying the original request shared across retries.
		// Use the request's original context as the base.
		ctx := context.WithValue(req.Context(), keyIndexContextKey, keyIndex)
		currentReq := req.Clone(ctx)

		// Restore the body for this attempt
		if len(bodyBytes) > 0 {
			currentReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			currentReq.ContentLength = int64(len(bodyBytes))
			currentReq.Header.Set("Content-Length", strconv.FormatInt(currentReq.ContentLength, 10))
		} else {
			// Ensure body is explicitly nil if no body bytes were read/buffered
			currentReq.Body = http.NoBody
			currentReq.ContentLength = 0
			currentReq.Header.Del("Content-Length") // Remove header if no body
		}

		// --- Apply Authentication ---
		useHeaderAuth := false
		for _, path := range rt.headerAuthPaths {
			if strings.Contains(currentReq.URL.Path, path) {
				useHeaderAuth = true
				break
			}
		}

		query := currentReq.URL.Query() // Get query parameters from the cloned request's URL
		if useHeaderAuth {
			log.Printf("[Retry Transport Attempt %d] Scope '%s': Using Authorization header (Key Index: %d)", attempt+1, scope, keyIndex)
			currentReq.Header.Set("Authorization", "Bearer "+apiKey)
			query.Del(rt.keyParam) // Remove query param if it exists
		} else {
			log.Printf("[Retry Transport Attempt %d] Scope '%s': Using query parameter '%s' (Key Index: %d)", attempt+1, scope, rt.keyParam, keyIndex)
			currentReq.Header.Del("Authorization") // Ensure Authorization header is removed
			query.Set(rt.keyParam, apiKey)
		}
		currentReq.URL.RawQuery = query.Encode() // Re-encode query parameters

		// Log outgoing request details (optional, can be verbose)
		// log.Printf("[Retry Transport Attempt %d] Scope '%s': Request URL: %s", attempt+1, scope, currentReq.URL.String())
		// log.Printf("[Retry Transport Attempt %d] Scope '%s': Request Headers: %v", attempt+1, scope, currentReq.Header)

		// --- Execute Request ---
		resp, lastErr = rt.underlyingTransport.RoundTrip(currentReq)

		// --- Check for Retry Conditions ---
		shouldRetry := false
		if lastErr != nil {
			log.Printf("[Retry Transport] Scope '%s': Attempt %d (Key Index %d) failed with transport error: %v", scope, attempt+1, keyIndex, lastErr)
			// Check if the error is temporary/network related
			if netErr, ok := lastErr.(net.Error); ok && netErr.Timeout() {
				shouldRetry = true
				log.Printf("[Retry Transport] Scope '%s': Network error is temporary, will retry.", scope)
			} else if errors.Is(lastErr, io.ErrUnexpectedEOF) || errors.Is(lastErr, io.EOF) {
				// Treat unexpected EOF as potentially temporary
				shouldRetry = true
				log.Printf("[Retry Transport] Scope '%s': EOF/UnexpectedEOF error, will retry.", scope)
			}
			// Note: No key marking needed here as the failure wasn't necessarily the key's fault.
		} else if resp.StatusCode == http.StatusTooManyRequests { // 429
			log.Printf("[Retry Transport] Scope '%s': Attempt %d (Key Index %d) failed with status %d (Too Many Requests)", scope, attempt+1, keyIndex, resp.StatusCode)
			shouldRetry = true
			rt.keyMan.markKeyFailed(scope, keyIndex) // Mark this key as failing for this scope
			// Consume and close response body before retrying
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		} else if resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented && resp.StatusCode != http.StatusHTTPVersionNotSupported {
			// Retry on 5xx server errors (except specific ones unlikely to change)
			log.Printf("[Retry Transport] Scope '%s': Attempt %d (Key Index %d) failed with status %d (Server Error)", scope, attempt+1, keyIndex, resp.StatusCode)
			shouldRetry = true
			// Don't mark key failed for 5xx, it's likely a server issue.
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		// --- Decide Action ---
		if !shouldRetry {
			// Success or non-retryable error/status code
			return resp, lastErr
		}

		// If we are about to retry, but it's the last attempt, break the loop
		// and return the current response/error.
		if attempt == maxRetries-1 {
			log.Printf("[Retry Transport] Max retries (%d) reached for scope '%s'. Returning last response/error.", maxRetries, scope)
			break
		}
	}

	// If loop finished, it means all retries were exhausted.
	// Return an error that includes the status code if the last attempt got a response.
	if lastErr == nil && resp != nil {
		// Last attempt got a response (e.g., 429, 5xx), but we're out of retries.
		finalErrorMsg := fmt.Sprintf("upstream server returned status %d after %d attempts (scope '%s')", resp.StatusCode, maxRetries, buildScopeKey(req.URL.Host, req.URL.Path))
		// Close the final response body as we are returning an error instead
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, &proxyErrorWithStatus{
			error:      errors.New(finalErrorMsg),
			StatusCode: resp.StatusCode,
		}
	}

	// Last attempt resulted in a transport error or key acquisition failed earlier.
	// If lastErr is nil here, it implies the initial key acquisition failed, which should be caught above.
	if lastErr == nil {
		lastErr = errors.New("internal error: retry loop exited without a final error or successful response")
		log.Printf("[Retry Transport] Scope '%s': %v", buildScopeKey(req.URL.Host, req.URL.Path), lastErr)
	}
	return nil, lastErr // Return the last transport error encountered
}

// isIdempotentMethod checks if a method is considered idempotent.
// Used to determine if the body needs buffering for retries.
// Note: This is a simplified check. PATCH can be non-idempotent.
func isIdempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}
