package configfile

import "sync/atomic"

// Current holds the configuration the service runs with: read once, at
// startup, and handed to every part of the service that reads it per
// request or per job.
type Current struct {
	file atomic.Pointer[File]
}

// NewCurrent holds the configuration loaded at startup.
func NewCurrent(f *File) *Current {
	c := &Current{}
	c.file.Store(f)
	return c
}

// Get returns the configuration.
func (c *Current) Get() *File {
	return c.file.Load()
}

// Set replaces the configuration. The service never calls it after
// startup; tests swap configurations with it.
func (c *Current) Set(f *File) {
	c.file.Store(f)
}
