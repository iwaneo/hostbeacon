package server

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// listener applies the listening rules before TLS: it drops connections from
// sources that are not allowed, caps connections that have not logged in with
// a key (in total and per source address), and gives them a short timeout.
type listener struct {
	net.Listener
	allows    func(netip.Addr) bool
	max       int
	perSource int
	timeout   time.Duration
	refused   func(source netip.Addr, reason string)

	mu       sync.Mutex
	total    int
	bySource map[netip.Addr]int
}

func (l *listener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		source := sourceOf(conn)
		if !l.allows(source) {
			l.refused(source, "not an allowed source address")
			conn.Close()
			continue
		}
		l.mu.Lock()
		full := l.total >= l.max || l.bySource[source] >= l.perSource
		if !full {
			l.total++
			l.bySource[source]++
		}
		l.mu.Unlock()
		if full {
			l.refused(source, "too many connections without a key")
			conn.Close()
			continue
		}
		conn.SetDeadline(time.Now().Add(l.timeout))
		return &countedConn{Conn: conn, listener: l, source: source}, nil
	}
}

func sourceOf(conn net.Conn) netip.Addr {
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		return addr.AddrPort().Addr().Unmap()
	}
	return netip.Addr{}
}

// countedConn is a connection that counts against the caps until it logs in
// or closes.
type countedConn struct {
	net.Conn
	listener *listener
	source   netip.Addr
	once     sync.Once
}

func (c *countedConn) release() {
	c.once.Do(func() {
		l := c.listener
		l.mu.Lock()
		defer l.mu.Unlock()
		l.total--
		if l.bySource[c.source]--; l.bySource[c.source] <= 0 {
			delete(l.bySource, c.source)
		}
	})
}

func (c *countedConn) Close() error {
	c.release()
	return c.Conn.Close()
}

// loggedIn stops counting the connection and lifts its timeout.
func (c *countedConn) loggedIn() {
	c.release()
	c.Conn.SetDeadline(time.Time{})
}
