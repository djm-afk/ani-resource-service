package server

import (
	"bytes"
	"sync"
)

// RPC completion logging can continue after the client receives its response.
// Tests take a synchronized snapshot while those server goroutines still run.
type safeLogBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *safeLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}
func (b *safeLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}
