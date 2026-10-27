package codex

import "testing"

// Regression tests: each one pins something that went wrong in real use.

// Which upstream errors are worth asking again (nothing was sent to the client
// yet) and which are answers: a wrong guess either hides a real failure or
// hammers a backend that said no.
func TestOverloadedClassification(t *testing.T) {
	again := [][2]string{
		{"server_is_overloaded", "service_unavailable_error"}, // seen at 15:59 and 16:52
		{"server_is_overloaded", ""},
		{"service_unavailable", ""},
		{"server_error", ""},
		{"", "service_unavailable_error"},
		{"", "server_error"},
	}
	for _, c := range again {
		if !overloaded(c[0], c[1]) {
			t.Errorf("code %q type %q: not retried", c[0], c[1])
		}
	}
	answers := [][2]string{
		{"", ""},
		{"context_length_exceeded", "invalid_request_error"},
		{"invalid_prompt", "invalid_request_error"},
		{"rate_limit_exceeded", "rate_limit_error"},
		{"insufficient_quota", "insufficient_quota"},
		{"model_not_found", ""},
	}
	for _, c := range answers {
		if overloaded(c[0], c[1]) {
			t.Errorf("code %q type %q: retried, but it is an answer", c[0], c[1])
		}
	}
}
