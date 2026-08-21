package resolver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// The resolver listens on the host loopback, on UDP and TCP alike: the
// guest reaches it through the provider's host alias, which the network
// provider translates to the host's loopback on the same port (ADR 0008).
// Binding the loopback and nothing else keeps it off the user's network.

const (
	// udpBufSize is the largest datagram we will read.
	udpBufSize = 4096
	// tcpIdleTimeout bounds a connection with nothing left to say.
	tcpIdleTimeout = 20 * time.Second
	// tcpQueryTimeout bounds one query/response exchange.
	tcpQueryTimeout = 30 * time.Second
	// queryTimeout bounds the work done for one query.
	queryTimeout = 15 * time.Second
	// portAttempts is how many ephemeral ports we try before giving up on
	// finding one free on UDP and TCP at once.
	portAttempts = 16
)

// Server serves one Handler on a UDP and a TCP listener sharing a port.
type Server struct {
	handler *Handler
	udp     net.PacketConn
	tcp     net.Listener
	wg      sync.WaitGroup
}

// Listen binds host on preferred (or, when that is 0 or taken, on a free
// ephemeral port) for UDP and TCP at once and returns the server.
func Listen(h *Handler, host string, preferred int) (*Server, error) {
	if preferred > 0 {
		if s, err := listenPort(h, host, preferred); err == nil {
			return s, nil
		}
	}
	var lastErr error
	for i := 0; i < portAttempts; i++ {
		pc, err := net.ListenPacket("udp4", net.JoinHostPort(host, "0"))
		if err != nil {
			return nil, fmt.Errorf("resolver: listening on %s: %w", host, err)
		}
		port := pc.LocalAddr().(*net.UDPAddr).Port
		ln, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			_ = pc.Close()
			lastErr = err
			continue
		}
		return &Server{handler: h, udp: pc, tcp: ln}, nil
	}
	return nil, fmt.Errorf("resolver: no port free on both udp and tcp after %d tries: %w", portAttempts, lastErr)
}

func listenPort(h *Handler, host string, port int) (*Server, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	return &Server{handler: h, udp: pc, tcp: ln}, nil
}

// Addr is the "host:port" both listeners are bound to.
func (s *Server) Addr() string { return s.udp.LocalAddr().String() }

// Port is the port both listeners are bound to.
func (s *Server) Port() int { return s.udp.LocalAddr().(*net.UDPAddr).Port }

// Close stops both listeners; Serve returns shortly afterwards.
func (s *Server) Close() error {
	err := errors.Join(s.udp.Close(), s.tcp.Close())
	s.wg.Wait()
	return err
}

// Serve answers queries until ctx is cancelled or the listeners are closed.
func (s *Server) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = s.udp.Close()
		_ = s.tcp.Close()
	}()
	errc := make(chan error, 2)
	go func() { errc <- s.serveUDP(ctx) }()
	go func() { errc <- s.serveTCP(ctx) }()
	err := <-errc
	cancel()
	<-errc
	s.wg.Wait()
	if ctx.Err() != nil && errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *Server) serveUDP(ctx context.Context) error {
	buf := make([]byte, udpBufSize)
	for {
		n, addr, err := s.udp.ReadFrom(buf)
		if err != nil {
			return err
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			qctx, cancel := context.WithTimeout(ctx, queryTimeout)
			defer cancel()
			reply := s.handler.Answer(qctx, query)
			if reply == nil {
				return
			}
			_ = s.udp.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, _ = s.udp.WriteTo(reply, addr)
		}()
	}
}

func (s *Server) serveTCP(ctx context.Context) error {
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.handleTCP(ctx, conn)
		}()
	}
}

func (s *Server) handleTCP(ctx context.Context, conn net.Conn) {
	var length [2]byte
	for {
		_ = conn.SetReadDeadline(time.Now().Add(tcpIdleTimeout))
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(length[:]))
		if n == 0 {
			return
		}
		query := make([]byte, n)
		_ = conn.SetReadDeadline(time.Now().Add(tcpQueryTimeout))
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		reply := s.handler.answerTCP(qctx, query)
		cancel()
		if reply == nil {
			return
		}
		binary.BigEndian.PutUint16(length[:], uint16(len(reply)))
		_ = conn.SetWriteDeadline(time.Now().Add(tcpQueryTimeout))
		if _, err := conn.Write(append(length[:], reply...)); err != nil {
			return
		}
	}
}
