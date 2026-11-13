package main

import (
	"crypto/tls"
	"net"
	"sync"
	"time"
)

type deadlineConn struct {
	net.Conn
	expires time.Time
	release func()
	once    sync.Once
}

func (c *deadlineConn) clamp(deadline time.Time) time.Time {
	if deadline.IsZero() || deadline.After(c.expires) {
		return c.expires
	}
	return deadline
}
func (c *deadlineConn) SetDeadline(t time.Time) error     { return c.Conn.SetDeadline(c.clamp(t)) }
func (c *deadlineConn) SetReadDeadline(t time.Time) error { return c.Conn.SetReadDeadline(c.clamp(t)) }
func (c *deadlineConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(c.clamp(t))
}
func (c *deadlineConn) Close() error {
	c.once.Do(func() {
		if c.release != nil {
			c.release()
		}
	})
	return c.Conn.Close()
}

type tlsListener struct {
	net.Listener
	config   *tls.Config
	slots    chan struct{}
	done     chan struct{}
	once     sync.Once
	closeErr error
	mu       sync.Mutex
	active   map[*deadlineConn]struct{}
}

func newTLSListener(listener net.Listener, config *tls.Config) *tlsListener {
	return &tlsListener{Listener: listener, config: config, slots: make(chan struct{}, maxConnections), done: make(chan struct{}), active: make(map[*deadlineConn]struct{})}
}
func (l *tlsListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	var bounded *deadlineConn
	bounded = &deadlineConn{Conn: conn, expires: time.Now().Add(sessionLifetime), release: func() {
		l.mu.Lock()
		delete(l.active, bounded)
		l.mu.Unlock()
		<-l.slots
	}}
	l.mu.Lock()
	l.active[bounded] = struct{}{}
	l.mu.Unlock()
	if err = bounded.SetDeadline(time.Time{}); err != nil {
		_ = bounded.Close()
		return nil, err
	}
	return tls.Server(bounded, l.config), nil
}
func (l *tlsListener) Close() error {
	l.once.Do(func() { close(l.done); l.closeErr = l.Listener.Close() })
	return l.closeErr
}

// closeConnections is called only after Accept has stopped and grace has expired.
func (l *tlsListener) closeConnections() {
	l.mu.Lock()
	active := make([]*deadlineConn, 0, len(l.active))
	for conn := range l.active {
		active = append(active, conn)
	}
	l.mu.Unlock()
	for _, conn := range active {
		_ = conn.Close()
	}
}
