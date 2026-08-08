// Package netrelay implements the high-throughput bidirectional stream copy
// used by SOCKS5 and HTTP CONNECT tunnels.
package netrelay

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const bufferSize = 128 * 1024

var bufferPool = sync.Pool{New: func() any {
	buffer := make([]byte, bufferSize)
	return &buffer
}}

type activityWriter struct {
	io.Writer
	counter *atomic.Uint64
	last    *atomic.Int64
}

func (w *activityWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.counter.Add(uint64(n))
		w.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// Bidirectional copies client-to-upstream and upstream-to-client concurrently.
// It uses pooled 128 KiB buffers and one shared idle watchdog. This avoids a
// SetDeadline syscall on every Read and Write, which is especially expensive
// when a TCP stream is passing through another SOCKS5 hop.
func Bidirectional(client, upstream net.Conn, idle time.Duration, upload, download *atomic.Uint64) error {
	if idle <= 0 {
		idle = 5 * time.Minute
	}
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	watchdog := time.NewTimer(idle)
	watchdogStop := make(chan struct{})
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		for {
			select {
			case <-watchdogStop:
				return
			case <-watchdog.C:
				remaining := idle - time.Since(time.Unix(0, lastActivity.Load()))
				if remaining <= 0 {
					_ = client.Close()
					_ = upstream.Close()
					return
				}
				watchdog.Reset(remaining)
			}
		}
	}()
	defer func() {
		watchdog.Stop()
		close(watchdogStop)
		<-watchdogDone
	}()

	errCh := make(chan error, 2)
	copyOne := func(dst, src net.Conn, counter *atomic.Uint64) {
		buffer := bufferPool.Get().(*[]byte)
		_, err := io.CopyBuffer(&activityWriter{Writer: dst, counter: counter, last: &lastActivity}, src, *buffer)
		bufferPool.Put(buffer)
		if closer, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		errCh <- err
	}
	go copyOne(upstream, client, upload)
	go copyOne(client, upstream, download)
	first := <-errCh
	if first != nil {
		_ = client.Close()
		_ = upstream.Close()
	}
	second := <-errCh
	_ = client.Close()
	_ = upstream.Close()
	if first != nil {
		return first
	}
	return second
}
