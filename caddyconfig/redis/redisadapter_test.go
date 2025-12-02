package redisadapter

import (
	"testing"
)

func TestCleanupEmptyParents(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		jsonPath string
		expected string
	}{
		{
			name:     "delete key from nested object",
			config:   `{"apps":{"http":{"servers":{"example":{"listen":["127.0.0.1:2019"]}}}}}`,
			jsonPath: "apps.http.servers.example.listen",
			expected: `{"apps":{"http":{"servers":{}}}}`,
		},
		{
			name:     "delete key makes parent empty object",
			config:   `{"apps":{"http":{"servers":{"example":{}}}}}`,
			jsonPath: "apps.http.servers.example",
			expected: `{"apps":{"http":{"servers":{}}}}`,
		},
		{
			name:     "delete nested key completely clears path",
			config:   `{"a":{"b":{"c":{"d":"value"}}}}`,
			jsonPath: "a.b.c.d",
			expected: `{"a":{"b":{"c":{}}}}`,
		},
		{
			name:     "delete key from empty array",
			config:   `{"apps":{"http":{"routes":[{"handle":"val"}]}}}`,
			jsonPath: "apps.http.routes.0",
			expected: `{"apps":{"http":{"routes":[]}}}`,
		},
		{
			name:     "delete last item from array makes it empty",
			config:   `{"apps":{"http":{"routes":[1]}}}`,
			jsonPath: "apps.http.routes.0",
			expected: `{"apps":{"http":{"routes":[]}}}`,
		},
		{
			name:     "delete key from nested structure with multiple empties",
			config:   `{"apps":{"http":{"servers":{"example":{"routes":[{"handle":{"path":"value"}}]}}}}}`,
			jsonPath: "apps.http.servers.example.routes.0.handle.path",
			expected: `{"apps":{"http":{"servers":{"example":{"routes":[{"handle":{}}]}}}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := cleanupEmptyParents(tt.config, tt.jsonPath)
			if err != nil {
				t.Errorf("cleanupEmptyParents() error = %v", err)
				return
			}
			if result != tt.expected {
				t.Errorf("cleanupEmptyParents() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestCleanupEmptyParentsMultipleLevels(t *testing.T) {
	// Test case where deleting one key causes multiple parent levels to become empty
	config := `{"apps":{"http":{"servers":{"example":{}}}}}`
	jsonPath := "apps.http.servers.example"
	
	result, err := cleanupEmptyParents(config, jsonPath)
	if err != nil {
		t.Fatalf("cleanupEmptyParents() error = %v", err)
	}
	
	expected := `{"apps":{"http":{"servers":{}}}}`
	if result != expected {
		t.Errorf("cleanupEmptyParents() = %v, want %v", result, expected)
	}
}