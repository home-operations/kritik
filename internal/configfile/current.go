package configfile

import "sync"

// Current holds the configuration the service runs with: read once, at
// startup (ADR-0022 §2.1), and handed to every part of the service that
// reads it per request or per job.
type Current struct {
	mu   sync.Mutex
	file *File
}

// NewCurrent holds the configuration loaded at startup.
func NewCurrent(f *File) *Current {
	return &Current{file: f}
}

// Get returns the configuration.
func (c *Current) Get() *File {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.file
}

// Set replaces the configuration. The service never calls it after
// startup; tests swap configurations with it.
func (c *Current) Set(f *File) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.file = f
}
