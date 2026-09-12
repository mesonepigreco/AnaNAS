package transferapi

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// LimitListener caps accepted connections, including idle clients and TLS
// handshakes. The caller must bind the underlying listener to the validated
// interface/address and wrap this listener with verified mutual TLS. This
// wrapper does not authorize a bind, route or automatic write.
func LimitListener(inner net.Listener, max int) (net.Listener, error) {
	if inner == nil || max < 1 || max > 8 {
		return nil, fmt.Errorf("listener and connection limit in [1,8] required")
	}
	return &limitedListener{Listener: inner, slots: make(chan struct{}, max), done: make(chan struct{})}, nil
}

type limitedListener struct {
	net.Listener
	slots    chan struct{}
	done     chan struct{}
	once     sync.Once
	closeErr error
}

func (l *limitedListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case l.slots <- struct{}{}:
	}
	connection, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		// No periodic kernel keepalive packets from an otherwise idle helper.
		if err := tcp.SetKeepAlive(false); err != nil {
			connection.Close()
			<-l.slots
			return nil, err
		}
	}
	return &limitedConn{Conn: connection, release: func() { <-l.slots }}, nil
}
func (l *limitedListener) Close() error {
	l.once.Do(func() { close(l.done); l.closeErr = l.Listener.Close() })
	return l.closeErr
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
	err     error
}

func (c *limitedConn) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close(); c.release() })
	return c.err
}

// HTTPServer supplies header/handshake bounds. TLS verification, the limited
// listener, operation pacing and per-request policy must all be retained by the
// deployment. No idle polling or heartbeat is introduced here.
func HTTPServer(handler *Server) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 * 1024, IdleTimeout: 15 * time.Second, TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
}
