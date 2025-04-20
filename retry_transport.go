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
		// --- Get API Key ---
		apiKey, currentKeyIndex, keyErr := rt.keyMan.getNextKey()
		if keyErr != nil {
			log.Printf("[Retry Transport] Error getting API key for attempt %d: %v", attempt+1, keyErr)
			// If we couldn't get a key, even on the first attempt, return the error.
			// If we had a previous error (e.g., from a failed request), return that instead?
			// Let's prioritize the key error as it prevents proceeding.
			if resp != nil {
				resp.Body.Close()
			}
			return nil, fmt.Errorf("failed to get API key (attempt %d): %w", attempt+1, keyErr)
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
		for _, prefix := range rt.headerAuthPaths {
			// Use currentReq.URL.Path which comes from the original request
			if strings.HasPrefix(currentReq.URL.Path, prefix) {
				useHeaderAuth = true
				break
			}
		}

		query := currentReq.URL.Query() // Get query parameters from the cloned request's URL
		if useHeaderAuth {
			// log.Printf("[Retry Transport Attempt %d] Using Authorization header for path: %s (Key Index: %d)", attempt+1, currentReq.URL.Path, keyIndex)
			currentReq.Header.Set("Authorization", "Bearer "+apiKey)
			query.Del(rt.keyParam) // Remove query param if it exists
		} else {
			// log.Printf("[Retry Transport Attempt %d] Using query parameter '%s' for path: %s (Key Index: %d)", attempt+1, rt.keyParam, currentReq.URL.Path, keyIndex)
			currentReq.Header.Del("Authorization") // Ensure Authorization header is removed
			query.Set(rt.keyParam, apiKey)
		}
		currentReq.URL.RawQuery = query.Encode() // Re-encode query parameters

		// Log outgoing request details (optional, can be verbose)
		// log.Printf("[Retry Transport Attempt %d] Request URL: %s", attempt+1, currentReq.URL.String())
		// log.Printf("[Retry Transport Attempt %d] Request Headers: %v", attempt+1, currentReq.Header)

		// --- Execute Request ---
		resp, lastErr = rt.underlyingTransport.RoundTrip(currentReq)

		// --- Check for Retry Conditions ---
		shouldRetry := false
		if lastErr != nil {
			log.Printf("[Retry Transport] Attempt %d (Key Index %d) failed with transport error: %v", attempt+1, keyIndex, lastErr)
			// Check if the error is temporary/network related
			if netErr, ok := lastErr.(net.Error); ok && (netErr.Timeout() || netErr.Temporary()) {
				shouldRetry = true
				log.Printf("[Retry Transport] Network error is temporary, will retry.")
			} else if errors.Is(lastErr, io.ErrUnexpectedEOF) || errors.Is(lastErr, io.EOF) {
				// Treat unexpected EOF as potentially temporary
				shouldRetry = true
				log.Printf("[Retry Transport] EOF/UnexpectedEOF error, will retry.")
			}
			// Note: No key marking needed here as the failure wasn't necessarily the key's fault.
		} else if resp.StatusCode == http.StatusTooManyRequests { // 429
			log.Printf("[Retry Transport] Attempt %d (Key Index %d) failed with status %d (Too Many Requests)", attempt+1, keyIndex, resp.StatusCode)
			shouldRetry = true
			rt.keyMan.markKeyFailed(keyIndex) // Mark this key as failing
			// Consume and close response body before retrying
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		} else if resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented && resp.StatusCode != http.StatusHTTPVersionNotSupported {
			// Retry on 5xx server errors (except specific ones unlikely to change)
			log.Printf("[Retry Transport] Attempt %d (Key Index %d) failed with status %d (Server Error)", attempt+1, keyIndex, resp.StatusCode)
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
			log.Printf("[Retry Transport] Max retries (%d) reached. Returning last response/error.", maxRetries)
			break
		}
	}

	// If loop finished, it means all retries were exhausted.
	// Return an error that includes the status code if the last attempt got a response.
	if lastErr == nil && resp != nil {
		// Last attempt got a response (e.g., 429, 5xx), but we're out of retries.
		finalErrorMsg := fmt.Sprintf("upstream server returned status %d after %d attempts", resp.StatusCode, maxRetries)
		// Close the final response body as we are returning an error instead
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, &proxyErrorWithStatus{
			error:      errors.New(finalErrorMsg),
			StatusCode: resp.StatusCode,
		}
	}

	// Last attempt resulted in a transport error or key acquisition failed earlier.
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
