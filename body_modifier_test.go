package main

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Helper function to create an io.ReadCloser from a string
func stringToReadCloser(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}

// Helper function to compare JSON objects ignoring order
func jsonDeepEqual(a, b []byte) bool {
	var objA, objB any
	if err := json.Unmarshal(a, &objA); err != nil {
		return false // Or handle error appropriately
	}
	if err := json.Unmarshal(b, &objB); err != nil {
		return false // Or handle error appropriately
	}
	return reflect.DeepEqual(objA, objB)
}

func TestHandlePostBody(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		addGoogleSearch bool
		searchTrigger   string
		wantBody        string
		wantErr         bool
	}{
		{
			name:            "addGoogleSearch false",
			body:            `{"key": "value"}`,
			addGoogleSearch: false,
			searchTrigger:   "search",
			wantBody:        `{"key": "value"}`,
			wantErr:         false,
		},
		{
			name:            "addGoogleSearch true, no trigger, no existing tools, no functions",
			body:            `{"contents": [{"parts": [{"text": "some content"}]}]}`,
			addGoogleSearch: true,
			searchTrigger:   "search",
			wantBody:        `{"contents": [{"parts": [{"text": "some content"}]}], "tools": [{"google_search":{}}]}`,
			wantErr:         false,
		},
		{
			name:            "addGoogleSearch true, with trigger",
			body:            `{"contents": [{"parts": [{"text": "please search the web"}]}]}`,
			addGoogleSearch: true,
			searchTrigger:   "search",
			wantBody:        `{"contents": [{"parts": [{"text": "please search the web"}]}], "tools": [{"google_search":{}}]}`,
			wantErr:         false,
		},
		// Add more test cases for handlePostBody if needed, like error handling for bad reader
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyReader := stringToReadCloser(tt.body) // Changed tt.tbody to tt.body
				gotBodyBytes, err := handlePostBody(bodyReader, tt.addGoogleSearch, tt.searchTrigger)

			if (err != nil) != tt.wantErr {
				t.Errorf("handlePostBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if !jsonDeepEqual(gotBodyBytes, []byte(tt.wantBody)) {
					t.Errorf("handlePostBody() gotBody = %s, want %s", string(gotBodyBytes), tt.wantBody)
				}
			}
		})
	}
}

func TestModifyBodyWithGoogleSearch(t *testing.T) {
	googleSearchToolJSON := `[{"google_search":{}}]`
	funcDeclarationsToolJSON := `[{"functionDeclarations": [{"name": "find_theaters"}]}]`

	tests := []struct {
		name          string
		bodyBytes     []byte
		searchTrigger string
		wantBodyBytes []byte
		wantErr       bool
	}{
		{
			name:          "invalid JSON",
			bodyBytes:     []byte(`not json`),
			searchTrigger: "search",
			wantBodyBytes: []byte(`not json`), // Should return original
			wantErr:       false,
		},
		{
			name:          "no trigger, no functionDeclarations, no tools",
			bodyBytes:     []byte(`{"contents": [{"parts": [{"text": "hello world"}]}]}`),
			searchTrigger: "search",
			wantBodyBytes: []byte(`{"contents": [{"parts": [{"text": "hello world"}]}], "tools": ` + googleSearchToolJSON + `}`),
			wantErr:       false,
		},
		{
			name:          "no trigger, no functionDeclarations, existing tools array without google_search",
			bodyBytes:     []byte(`{"contents": [], "tools": [{"some_other_tool":{}}]}`),
			searchTrigger: "search",
			wantBodyBytes: []byte(`{"contents": [], "tools": [{"some_other_tool":{}}, {"google_search":{}}]}`),
			wantErr:       false,
		},
		{
			name:          "no trigger, no functionDeclarations, existing tools array with google_search",
			bodyBytes:     []byte(`{"contents": [], "tools": [{"google_search":{}}]}`),
			searchTrigger: "search",
			wantBodyBytes: []byte(`{"contents": [], "tools": [{"google_search":{}}]}`), // Should not modify
			wantErr:       false,
		},
		{
			name:          "no trigger, with functionDeclarations",
			bodyBytes:     []byte(`{"contents": [], "tools": ` + funcDeclarationsToolJSON + `}`),
			searchTrigger: "search",
			wantBodyBytes: []byte(`{"contents": [], "tools": ` + funcDeclarationsToolJSON + `}`), // Should not modify
			wantErr:       false,
		},
        {
            name:          "trigger found (exact word, case-insensitive), no existing tools",
            bodyBytes:     []byte(`{"contents": [{"parts": [{"text": "Please SeArCh the web."}]}]}`),
            searchTrigger: "search",
            wantBodyBytes: []byte(`{"contents": [{"parts": [{"text": "Please SeArCh the web."}]}], "tools": ` + googleSearchToolJSON + `}`),
            wantErr:       false,
        },
		{
			name:          "trigger found (exact word), existing functionDeclarations",
			bodyBytes:     []byte(`{"contents": [{"parts": [{"text": "search now"}]}], "tools": ` + funcDeclarationsToolJSON + `}`),
			searchTrigger: "search",
			wantBodyBytes: []byte(`{"contents": [{"parts": [{"text": "search now"}]}], "tools": ` + googleSearchToolJSON + `}`), // Should replace tools
			wantErr:       false,
		},
        {
            name:          "trigger found (exact word), existing tools map with functionDeclarations",
            bodyBytes:     []byte(`{"contents": [{"parts": [{"text": "search now"}]}], "tools": {"functionDeclarations": [{"name": "find_theaters"}], "other_stuff": 1}}`),
            searchTrigger: "search",
            wantBodyBytes: []byte(`{"contents": [{"parts": [{"text": "search now"}]}], "tools": {"google_search": {}, "other_stuff": 1}}`), // Should remove FD, add GS
            wantErr:       false,
        },
        {
            name:          "trigger found (exact word), existing tools map without functionDeclarations",
            bodyBytes:     []byte(`{"contents": [{"parts": [{"text": "search now"}]}], "tools": {"other_stuff": 1}}`),
            searchTrigger: "search",
            wantBodyBytes: []byte(`{"contents": [{"parts": [{"text": "search now"}]}], "tools": {"google_search": {}, "other_stuff": 1}}`), // Should add GS
            wantErr:       false,
        },
		{
			name:          "trigger found but as substring, not whole word",
			bodyBytes:     []byte(`{"contents": [{"parts": [{"text": "researching this topic"}]}]}`),
			searchTrigger: "search",
			wantBodyBytes: []byte(`{"contents": [{"parts": [{"text": "researching this topic"}]}], "tools": ` + googleSearchToolJSON + `}`), // No trigger, should add GS
			wantErr:       false,
		},
		{
			name:          "trigger not found",
			bodyBytes:     []byte(`{"contents": [{"parts": [{"text": "hello there"}]}]}`),
			searchTrigger: "search",
			wantBodyBytes: []byte(`{"contents": [{"parts": [{"text": "hello there"}]}], "tools": ` + googleSearchToolJSON + `}`),
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBodyBytes, err := modifyBodyWithGoogleSearch(tt.bodyBytes, tt.searchTrigger)
			if (err != nil) != tt.wantErr {
				t.Errorf("modifyBodyWithGoogleSearch() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			// For non-JSON, compare strings directly
			if json.Valid(tt.wantBodyBytes) && json.Valid(gotBodyBytes) {
				if !jsonDeepEqual(gotBodyBytes, tt.wantBodyBytes) {
					t.Errorf("modifyBodyWithGoogleSearch() JSON mismatch: gotBody = %s, want %s", string(gotBodyBytes), string(tt.wantBodyBytes))
				}
			} else if !bytes.Equal(gotBodyBytes, tt.wantBodyBytes) {
				t.Errorf("modifyBodyWithGoogleSearch() Non-JSON mismatch: gotBody = %s, want %s", string(gotBodyBytes), string(tt.wantBodyBytes))
			}
		})
	}
}
