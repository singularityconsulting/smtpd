// Package smtpd implements an SMTP server with support for STARTTLS, authentication (PLAIN/LOGIN), XCLIENT and optional restrictions on the different stages of the SMTP session.
package smtpd

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pires/go-proxyproto"
	"go.uber.org/zap"
)

// Server defines the parameters for running the SMTP server
type Server struct {
	Hostname       string // Server hostname. (default: "localhost.localdomain")
	WelcomeMessage string // Initial server banner. (default: "<hostname> ESMTP ready.")

	ReadTimeout  time.Duration // Socket timeout for read operations. (default: 60s)
	WriteTimeout time.Duration // Socket timeout for write operations. (default: 60s)
	DataTimeout  time.Duration // Socket timeout for DATA command (default: 5m)

	MaxConnections int // Max concurrent connections, use -1 to disable. (default: 100)
	MaxMessageSize int // Max message size in bytes. (default: 10240000)
	MaxRecipients  int // Max RCPT TO calls for each envelope. (default: 100)

	SleepTime time.Duration // Time to sleep if we don't get a new connection. (default: 1s)

	// New e-mails are handed off to this function.
	// Can be left empty for a NOOP server.
	// If an error is returned, it will be reported in the SMTP session.
	Handler func(peer Peer, env Envelope) error

	// Enable various checks during the SMTP session.
	// Can be left empty for no restrictions.
	// If an error is returned, it will be reported in the SMTP session.
	// Use the Error struct for access to error codes.
	ConnectionChecker func(peer Peer) error              // Called upon new connection.
	HeloChecker       func(peer Peer, name string) error // Called after HELO/EHLO.
	SenderChecker     func(peer Peer, addr string) error // Called after MAIL FROM.
	RecipientChecker  func(peer Peer, addr string) error // Called after each RCPT TO.

	// Enable PLAIN/LOGIN authentication, only available after STARTTLS.
	// Can be left empty for no authentication support.
	Authenticator func(peer Peer, username, password string) error

	EnableXCLIENT       bool // Enable XCLIENT support (default: false)
	EnableProxyProtocol bool // Enable proxy protocol support (default: false)

	// ReadHeaderTimeout is how long header processing waits for header to
	// be read from the wire. (default: 10s)
	ReadHeaderTimeout time.Duration

	TLSConfig *tls.Config // Enable STARTTLS support.
	ForceTLS  bool        // Force STARTTLS usage.

	ProtocolLogger *log.Logger

	ZapLogger *zap.Logger

	// mu guards doneChan and makes closing it and listener atomic from
	// perspective of Serve()
	mu         sync.Mutex
	doneChan   chan struct{}
	listener   *net.Listener
	waitgrp    sync.WaitGroup
	inShutdown atomicBool // true when server is in shutdown

	// We need a way to track the protocol return codes
	ReplyHandler func(peer Peer, code int, message string)

	// And a way to track the end for each request so we can get metrics
	CloseSessionHandler func(peer Peer)

	// Function to track the time it takes each step of the protocol
	ProtocolTrackerHandler func(peer Peer, action string, duration time.Duration)

	// On session close deadline
	CloseMaxDeadline time.Duration
}

// Protocol represents the protocol used in the SMTP session
type Protocol string

const (
	// SMTP
	SMTP Protocol = "SMTP"

	// Extended SMTP
	ESMTP = "ESMTP"
)

// Peer represents the client connecting to the server
type Peer struct {
	HeloName    string               // Server name used in HELO/EHLO command
	Username    string               // Username from authentication, if authenticated
	Password    string               // Password from authentication, if authenticated
	Protocol    Protocol             // Protocol used, SMTP or ESMTP
	ServerName  string               // A copy of Server.Hostname
	Addr        net.Addr             // Network address
	TLS         *tls.ConnectionState // TLS Connection details, if on TLS
	InitTime    time.Time            // Time when the struct was created
	MailCounter int64                // Counts the number of mails we get per connection
	ID          string               // A UUID assigned for this session
}

// Error represents an Error reported in the SMTP session.
type Error struct {
	Code    int    // The integer error code
	Message string // The error message
}

// Error returns a string representation of the SMTP error
func (e Error) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Message) }

// ErrServerClosed is returned by the Server's Serve and ListenAndServe,
// methods after a call to Shutdown.
var ErrServerClosed = errors.New("smtp: Server closed")

type session struct {
	server *Server

	peer     Peer
	envelope *Envelope

	conn net.Conn

	reader  *bufio.Reader
	writer  *bufio.Writer
	scanner *bufio.Scanner

	tls bool

	isClosed bool
}

func (srv *Server) newSession(c net.Conn) (s *session) {
	sessionID := ""

	id, err := uuid.NewUUID()

	if err != nil {
		log.Printf("Error generating UUID for new smtp session: %v", err)
	} else {
		sessionID = id.String()
	}

	s = &session{
		server:   srv,
		conn:     c,
		reader:   bufio.NewReader(c),
		writer:   bufio.NewWriter(c),
		isClosed: false,
		peer: Peer{
			Addr:        c.RemoteAddr(),
			ServerName:  srv.Hostname,
			InitTime:    time.Now().UTC(),
			MailCounter: 0,
			ID:          sessionID,
		},
	}

	// Check if the underlying connection is already TLS.
	// This will happen if the Listerner provided Serve()
	// is from tls.Listen()

	var tlsConn *tls.Conn

	tlsConn, s.tls = c.(*tls.Conn)

	if s.tls {
		// run handshake otherwise it's done when we first
		// read/write and connection state will be invalid
		tlsConn.Handshake()
		state := tlsConn.ConnectionState()
		s.peer.TLS = &state
	}

	s.scanner = bufio.NewScanner(s.reader)

	return

}

// ListenAndServe starts the SMTP server and listens on the address provided
func (srv *Server) ListenAndServe(addr string) error {
	if srv.shuttingDown() {
		return ErrServerClosed
	}

	srv.configureDefaults()

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	return srv.Serve(l)
}

// Serve starts the SMTP server and listens on the Listener provided
func (srv *Server) Serve(l net.Listener) error {
	if srv.shuttingDown() {
		return ErrServerClosed
	}

	srv.configureDefaults()

	if srv.EnableProxyProtocol {
		l = &proxyproto.Listener{
			Listener:          l,
			ReadHeaderTimeout: srv.ReadHeaderTimeout,
		}
	}

	l = &onceCloseListener{Listener: l}
	defer l.Close()

	srv.listener = &l

	var limiter chan struct{}

	if srv.MaxConnections > 0 {
		limiter = make(chan struct{}, srv.MaxConnections)
	}

	for {
		conn, e := l.Accept()
		if e != nil {
			select {
			case <-srv.getDoneChan():
				return ErrServerClosed
			default:
			}

			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				time.Sleep(srv.SleepTime)
				continue
			}
			return e
		}

		session := srv.newSession(conn)

		srv.waitgrp.Add(1)
		go func() {
			defer srv.waitgrp.Done()
			if limiter != nil {
				select {
				case limiter <- struct{}{}:
					session.serve()
					<-limiter
				default:
					session.reject()
				}
			} else {
				session.serve()
			}
		}()
	}

}

// Shutdown instructs the server to shutdown, starting by closing the
// associated listener. If wait is true, it will wait for the shutdown
// to complete. If wait is false, Wait must be called afterwards.
func (srv *Server) Shutdown(wait bool) error {
	var lnerr error
	srv.inShutdown.setTrue()

	// First close the listener
	srv.mu.Lock()
	if srv.listener != nil {
		lnerr = (*srv.listener).Close()
	}
	srv.closeDoneChanLocked()
	srv.mu.Unlock()

	// Now wait for all client connections to close
	if wait {
		srv.Wait()
	}

	return lnerr
}

// Wait waits for all client connections to close and the server to finish
// shutting down.
func (srv *Server) Wait() error {
	if !srv.shuttingDown() {
		return errors.New("Server has not been Shutdown")
	}

	srv.waitgrp.Wait()
	return nil
}

// Address returns the listening address of the server
func (srv *Server) Address() net.Addr {
	return (*srv.listener).Addr()
}

func (srv *Server) configureDefaults() {
	if srv.SleepTime == 0 {
		srv.SleepTime = time.Second
	}

	if srv.CloseMaxDeadline == 0 {
		srv.CloseMaxDeadline = 200 * time.Millisecond
	}

	if srv.MaxMessageSize == 0 {
		srv.MaxMessageSize = 10240000
	}

	if srv.MaxConnections == 0 {
		srv.MaxConnections = 100
	}

	if srv.MaxRecipients == 0 {
		srv.MaxRecipients = 100
	}

	if srv.ReadTimeout == 0 {
		srv.ReadTimeout = time.Second * 60
	}

	if srv.WriteTimeout == 0 {
		srv.WriteTimeout = time.Second * 60
	}

	if srv.DataTimeout == 0 {
		srv.DataTimeout = time.Minute * 5
	}

	if srv.ForceTLS && srv.TLSConfig == nil {
		log.Fatal("Cannot use ForceTLS with no TLSConfig")
	}

	if srv.Hostname == "" {
		srv.Hostname = "localhost.localdomain"
	}

	if srv.WelcomeMessage == "" {
		srv.WelcomeMessage = fmt.Sprintf("%s ESMTP ready.", srv.Hostname)
	}

	if srv.ReadHeaderTimeout == 0 {
		srv.ReadHeaderTimeout = time.Second * 10
	}
}

func (session *session) serve() {

	defer session.close()

	session.welcome()

	for {

		for session.scanner.Scan() {
			line := session.scanner.Text()
			session.logf("received: %s", strings.TrimSpace(line))
			session.handle(line)
		}

		err := session.scanner.Err()

		if err == bufio.ErrTooLong {

			session.reply(500, "Line too long")

			// Advance reader to the next newline

			session.reader.ReadString('\n')
			session.scanner = bufio.NewScanner(session.reader)

			// Reset and have the client start over.

			session.reset()

			continue
		}

		break
	}

}

func (session *session) reject() {
	session.reply(421, "Too busy. Try again later.")
	session.closeConnection()
}

func (session *session) reset() {
	session.envelope = nil
}

func (session *session) welcome() {

	if session.server.ConnectionChecker != nil {
		err := session.server.ConnectionChecker(session.peer)
		if err != nil {
			session.error(err)
			session.close()
			return
		}
	}

	session.reply(220, session.server.WelcomeMessage)

}

func (session *session) reply(code int, message string) {
	session.logf("sending: %d %s", code, message)
	fmt.Fprintf(session.writer, "%d %s\r\n", code, message)
	session.flush()

	if session.server.ReplyHandler != nil {
		session.server.ReplyHandler(session.peer, code, message)
	}
}

func (session *session) flush() {
	session.conn.SetWriteDeadline(time.Now().Add(session.server.WriteTimeout))
	session.writer.Flush()
	session.conn.SetReadDeadline(time.Now().Add(session.server.ReadTimeout))
}

func (session *session) error(err error) {
	if smtpdError, ok := err.(Error); ok {
		session.reply(smtpdError.Code, smtpdError.Message)
	} else {
		session.reply(502, fmt.Sprintf("%s", err))
	}
}

func (session *session) logf(format string, v ...interface{}) {
	if session.server.ProtocolLogger == nil {
		return
	}
	session.server.ProtocolLogger.Output(2, fmt.Sprintf(
		"%s [peer:%s]",
		fmt.Sprintf(format, v...),
		session.peer.Addr,
	))

}

func (session *session) logError(err error, desc string) {
	session.logf("%s: %v ", desc, err)
}

func (session *session) extensions() []string {

	extensions := []string{
		fmt.Sprintf("SIZE %d", session.server.MaxMessageSize),
		"8BITMIME",
		"PIPELINING",
	}

	if session.server.EnableXCLIENT {
		extensions = append(extensions, "XCLIENT")
	}

	if session.server.TLSConfig != nil && !session.tls {
		extensions = append(extensions, "STARTTLS")
	}

	if session.server.Authenticator != nil && session.tls {
		extensions = append(extensions, "AUTH PLAIN LOGIN")
	}

	return extensions

}

func (session *session) deliver() error {
	if session.server.Handler != nil {
		return session.server.Handler(session.peer, *session.envelope)
	}
	return nil
}

func (session *session) close() {
	if !session.isClosed {
		session.closeConnection()
		if session.server.CloseSessionHandler != nil {
			session.server.CloseSessionHandler(session.peer)
		}
	}
}

func (session *session) closeConnection() {
	session.isClosed = true

	session.conn.SetWriteDeadline(time.Now().Add(session.server.CloseMaxDeadline))

	session.writer.Flush()
	// trying to avoid having a hardcoded sleep time to close the connection using SetWriteDeadline
	// time.Sleep(200 * time.Millisecond)

	session.conn.Close()
}

// From net/http/server.go

func (s *Server) shuttingDown() bool {
	return s.inShutdown.isSet()
}

func (s *Server) getDoneChan() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getDoneChanLocked()
}

func (s *Server) getDoneChanLocked() chan struct{} {
	if s.doneChan == nil {
		s.doneChan = make(chan struct{})
	}
	return s.doneChan
}

func (s *Server) closeDoneChanLocked() {
	ch := s.getDoneChanLocked()
	select {
	case <-ch:
		// Already closed. Don't close again.
	default:
		// Safe to close here. We're the only closer, guarded
		// by s.mu.
		close(ch)
	}
}

// onceCloseListener wraps a net.Listener, protecting it from
// multiple Close calls.
type onceCloseListener struct {
	net.Listener
	once     sync.Once
	closeErr error
}

func (oc *onceCloseListener) Close() error {
	oc.once.Do(oc.close)
	return oc.closeErr
}

func (oc *onceCloseListener) close() { oc.closeErr = oc.Listener.Close() }

type atomicBool int32

func (b *atomicBool) isSet() bool { return atomic.LoadInt32((*int32)(b)) != 0 }
func (b *atomicBool) setTrue()    { atomic.StoreInt32((*int32)(b), 1) }
func (b *atomicBool) setFalse()   { atomic.StoreInt32((*int32)(b), 0) }
