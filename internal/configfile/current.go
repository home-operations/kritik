package configfile

import "sync"

// Current holds the last good File and lets one consumer wait for the next
// replacement. It is the hand-off between Watch, which produces files, and
// the leader, which applies them.
type Current struct {
	mu      sync.Mutex
	file    *File
	changed chan struct{}
}

// NewCurrent starts from the file loaded at startup.
func NewCurrent(f *File) *Current {
	return &Current{file: f, changed: make(chan struct{})}
}

// Get returns the last good file.
func (c *Current) Get() *File {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.file
}

// Set replaces the file and wakes anyone waiting on Changed.
func (c *Current) Set(f *File) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.file = f
	close(c.changed)
	c.changed = make(chan struct{})
}

// Changed returns a channel closed on the next Set. Call Get after it fires.
func (c *Current) Changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}
