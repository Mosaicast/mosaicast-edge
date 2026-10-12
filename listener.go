// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package main

import (
	"net"
	"sync"
)

// limitListener holds at most max connections open at once. Further connections wait in the kernel's
// accept queue until one closes, rather than being accepted and costing memory: every open connection
// is a goroutine and buffers (~20 KB), and a crowd must slow the edge down, not take it out of memory.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func limitListen(l net.Listener, max int) net.Listener {
	return &limitListener{Listener: l, sem: make(chan struct{}, max)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
