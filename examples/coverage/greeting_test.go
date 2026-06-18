package greeting

import "testing"

// TestGreet exercises Greet (both branches) but never calls Farewell, so the
// coverage report will show Greet as covered and Farewell as uncovered.
func TestGreet(t *testing.T) {
	if got := Greet("Bazel"); got != "Hello, Bazel!" {
		t.Errorf("Greet(%q) = %q, want %q", "Bazel", got, "Hello, Bazel!")
	}
	if got := Greet(""); got != "Hello, stranger!" {
		t.Errorf("Greet(%q) = %q, want %q", "", got, "Hello, stranger!")
	}
}
