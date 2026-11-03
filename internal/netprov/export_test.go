package netprov

// unregister removes a test registration, so tests that Register can run
// more than once in one process (go test -count=N).
func unregister(name string) {
	mu.Lock()
	defer mu.Unlock()
	delete(registry, name)
}
