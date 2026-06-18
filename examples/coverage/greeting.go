package greeting

import "fmt"

// Greet returns a greeting for name. It is exercised by the test, so its
// lines show up as covered in the coverage report.
func Greet(name string) string {
	if name == "" {
		return "Hello, stranger!"
	}
	return fmt.Sprintf("Hello, %s!", name)
}

// Farewell is intentionally never called by the test, so its lines show up
// as uncovered (count 0) in the coverage report.
func Farewell(name string) string {
	if name == "" {
		return "Goodbye, stranger!"
	}
	return fmt.Sprintf("Goodbye, %s!", name)
}
