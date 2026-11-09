package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

// TestMaxTokensParameterErrorDetection tests the error detection logic for parameter errors
func TestMaxTokensParameterErrorDetection(t *testing.T) {
	tests := []struct {
		name         string
		errorMsg     string
		shouldDetect bool
		description  string
	}{
		{
			name:         "max_completion_tokens unsupported",
			errorMsg:     "Error: unsupported parameter 'max_completion_tokens'",
			shouldDetect: true,
			description:  "Should detect unsupported max_completion_tokens",
		},
		{
			name:         "max_tokens invalid",
			errorMsg:     "invalid parameter: max_tokens not supported for this model",
			shouldDetect: true,
			description:  "Should detect invalid max_tokens",
		},
		{
			name:         "parameter error lowercase",
			errorMsg:     "parameter 'max_completion_tokens' is not supported",
			shouldDetect: true,
			description:  "Should handle lowercase parameter errors",
		},
		{
			name:         "OpenWebUI style error",
			errorMsg:     "500: 'max_completion_tokens' is not a valid OpenAI parameter",
			shouldDetect: true,
			description:  "Should detect OpenWebUI style errors",
		},
		{
			name:         "unrelated error",
			errorMsg:     "Authentication failed",
			shouldDetect: false,
			description:  "Should not detect unrelated errors",
		},
		{
			name:         "timeout error",
			errorMsg:     "request timeout",
			shouldDetect: false,
			description:  "Should not detect timeout errors",
		},
		{
			name:         "empty error message",
			errorMsg:     "",
			shouldDetect: false,
			description:  "Should handle empty error messages",
		},
		{
			name:         "wrong parameter name",
			errorMsg:     "unsupported parameter 'max_input_tokens'",
			shouldDetect: false,
			description:  "Should not detect errors for wrong parameters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isMaxTokensParameterError(tt.errorMsg)
			if result != tt.shouldDetect {
				t.Errorf("%s: got %v, want %v", tt.description, result, tt.shouldDetect)
			}
		})
	}
}

func TestModelCapabilityCacheStructure(t *testing.T) {
	scope := t.Name() + "/" + t.TempDir()
	key := config.CacheKey{
		BaseURL: "https://api.openai.com/v1/" + scope,
		Model:   "gpt-5",
	}
	reconstructed := config.CacheKey{
		BaseURL: "https://api.openai.com/v1/" + scope,
		Model:   "gpt-5",
	}
	otherProvider := config.CacheKey{
		BaseURL: "https://openrouter.ai/api/v1/" + scope,
		Model:   "gpt-5",
	}

	want := &config.ModelCapabilities{UsesMaxCompletionTokens: true}
	config.SetModelCapabilities(key, want)
	got := config.GetModelCapabilities(reconstructed)
	if got == nil {
		t.Fatal("same provider and model should retrieve the stored cache entry")
	}
	if !got.UsesMaxCompletionTokens {
		t.Fatal("retrieved cache entry has the wrong capability")
	}
	if got := config.GetModelCapabilities(otherProvider); got != nil {
		t.Fatalf("different provider unexpectedly retrieved cache entry: %+v", got)
	}
}

func TestCacheHitAndMiss(t *testing.T) {
	scope := t.Name() + "/" + t.TempDir()
	key := config.CacheKey{
		BaseURL: "https://api.openai.com/v1/" + scope,
		Model:   "gpt-5",
	}

	if caps := config.GetModelCapabilities(key); caps != nil {
		t.Fatalf("new cache key unexpectedly hit: %+v", caps)
	}

	want := &config.ModelCapabilities{UsesMaxCompletionTokens: true}
	config.SetModelCapabilities(key, want)

	got := config.GetModelCapabilities(key)
	if got == nil {
		t.Fatal("cache should hit after setting capabilities")
	}
	if !got.UsesMaxCompletionTokens {
		t.Fatalf("cached capability = %v, want true", got.UsesMaxCompletionTokens)
	}
}

func TestPerModelPerProviderScoping(t *testing.T) {
	scope := t.Name() + "/" + t.TempDir()
	providerA := "https://api.openai.com/v1/" + scope
	providerB := "https://openrouter.ai/api/v1/" + scope
	entries := []struct {
		key   config.CacheKey
		value bool
	}{
		{config.CacheKey{BaseURL: providerA, Model: "shared-model"}, true},
		{config.CacheKey{BaseURL: providerB, Model: "shared-model"}, false},
		{config.CacheKey{BaseURL: providerA, Model: "different-model"}, false},
	}

	for _, entry := range entries {
		config.SetModelCapabilities(entry.key, &config.ModelCapabilities{
			UsesMaxCompletionTokens: entry.value,
		})
	}
	for _, entry := range entries {
		got := config.GetModelCapabilities(entry.key)
		if got == nil {
			t.Errorf("cache miss for provider %q, model %q", entry.key.BaseURL, entry.key.Model)
			continue
		}
		if got.UsesMaxCompletionTokens != entry.value {
			t.Errorf("provider %q, model %q capability = %v, want %v",
				entry.key.BaseURL, entry.key.Model, got.UsesMaxCompletionTokens, entry.value)
		}
	}
}

func TestCacheConcurrency(t *testing.T) {
	const workers, iterations = 10, 100
	scope := t.Name() + "/" + t.TempDir()
	keys := make([]config.CacheKey, workers)
	wants := make([]bool, workers)
	for i := range keys {
		keys[i] = config.CacheKey{
			BaseURL: "https://api.openai.com/v1/" + scope,
			Model:   fmt.Sprintf("model-%d", i),
		}
		wants[i] = i%2 == 0
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errors := make(chan string, workers*iterations)
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				// Each call publishes a fresh immutable value.
				config.SetModelCapabilities(keys[id], &config.ModelCapabilities{
					UsesMaxCompletionTokens: wants[id],
				})
			}
		}(i)
		go func(id int) {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				got := config.GetModelCapabilities(keys[id])
				if got == nil {
					continue // The corresponding writer may not have published yet.
				}
				if got.UsesMaxCompletionTokens != wants[id] {
					errors <- fmt.Sprintf("model %q capability = %v, want %v",
						keys[id].Model, got.UsesMaxCompletionTokens, wants[id])
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}

	for i, key := range keys {
		got := config.GetModelCapabilities(key)
		if got == nil {
			t.Errorf("no final cache value for model %q", key.Model)
			continue
		}
		if got.UsesMaxCompletionTokens != wants[i] {
			t.Errorf("final model %q capability = %v, want %v", key.Model, got.UsesMaxCompletionTokens, wants[i])
		}
	}
}

func TestCacheTimestampTracking(t *testing.T) {
	scope := t.Name() + "/" + t.TempDir()
	key := config.CacheKey{
		BaseURL: "https://api.openai.com/v1/" + scope,
		Model:   "gpt-5",
	}
	ancient := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

	config.SetModelCapabilities(key, &config.ModelCapabilities{
		UsesMaxCompletionTokens: true,
		LastChecked:             ancient,
	})
	previous := config.GetModelCapabilities(key)
	if previous == nil {
		t.Fatal("cache entry missing after initial set")
	}
	previousChecked := previous.LastChecked

	before := time.Now()
	config.SetModelCapabilities(key, &config.ModelCapabilities{
		UsesMaxCompletionTokens: false,
		LastChecked:             ancient,
	})
	after := time.Now()
	got := config.GetModelCapabilities(key)
	if got == nil {
		t.Fatal("cache entry missing after replacement")
	}
	if got.UsesMaxCompletionTokens {
		t.Fatal("replacement did not store the new capability value")
	}
	if got.LastChecked.Before(before) || got.LastChecked.After(after) {
		t.Errorf("replacement LastChecked = %v, want between %v and %v", got.LastChecked, before, after)
	}
	if got.LastChecked.Before(previousChecked) {
		t.Errorf("replacement LastChecked = %v, want not earlier than prior value %v", got.LastChecked, previousChecked)
	}
}

// TestErrorDetectionBroadMatching tests broad keyword matching for various error formats
func TestErrorDetectionBroadMatching(t *testing.T) {
	// These should ALL match: require both parameter indicator AND parameter name
	shouldMatch := []string{
		"parameter 'max_completion_tokens'",
		"Parameter 'max_completion_tokens'",
		"parameter max_tokens",
		"invalid parameter max_completion_tokens",
		"unsupported parameter max_tokens",
		"Parameter 'MAX_COMPLETION_TOKENS' is invalid",
		"UNSUPPORTED parameter max_tokens",
		"OpenAI error: parameter max_completion_tokens unsupported",
		"OpenWebUI: parameter 'max_completion_tokens' not supported",
		"LiteLLM error: invalid parameter max_tokens",
		"Invalid max_tokens parameter",
		"Unsupported max_completion_tokens",
	}

	for _, pattern := range shouldMatch {
		if !isMaxTokensParameterError(pattern) {
			t.Errorf("Should detect parameter error: %s", pattern)
		}
	}

	// These should NOT match: missing either indicator or parameter name
	shouldNotMatch := []string{
		"parameter 'max_input_tokens'", // wrong parameter
		"parameter mismatch",           // no token param
		"max_tokens is great",          // no error indicator
		"invalid request",              // no token param
		"unsupported feature",          // no token param
		"",                             // empty
	}

	for _, pattern := range shouldNotMatch {
		if isMaxTokensParameterError(pattern) {
			t.Errorf("Should NOT detect parameter error: %s", pattern)
		}
	}
}
