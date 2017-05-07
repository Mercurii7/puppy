package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deckarep/golang-set"
)

/*
func logReq(req *http.Request, logger *log.Logger) {
	buf := new(bytes.Buffer)
	req.Write(buf)
	s := buf.String()
	logger.Print(s)
}
*/

const (
	ProxyStopped = iota
	ProxyStarting
	ProxyRunning
)

var getNextConnId = IdCounter()
var getNextListenerId = IdCounter()

/*
A type representing the "Addr" for our internal connections
*/
type InternalAddr struct{}

func (InternalAddr) Network() string {
	return "<internal network>"
}

func (InternalAddr) String() string {
	return "<internal connection>"
}

/*
ProxyConn which is the same as a net.Conn but implements Peek() and variales to store target host data
*/
type ProxyConn interface {
	net.Conn

	Id() int
	Logger() *log.Logger

	SetCACertificate(*tls.Certificate)
	StartMaybeTLS(hostname string) (bool, error)
}

type proxyAddr struct {
	Host   string
	Port   int // can probably do a uint16 or something but whatever
	UseTLS bool
}

type proxyConn struct {
	Addr    *proxyAddr
	logger  *log.Logger
	id      int
	conn    net.Conn      // Wrapped connection
	readReq *http.Request // A replaced request
	caCert  *tls.Certificate
}

// ProxyAddr implementations/functions

func EncodeRemoteAddr(host string, port int, useTLS bool) string {
	var tlsInt int
	if useTLS {
		tlsInt = 1
	} else {
		tlsInt = 0
	}
	return fmt.Sprintf("%s/%d/%d", host, port, tlsInt)
}

func DecodeRemoteAddr(addrStr string) (host string, port int, useTLS bool, err error) {
	parts := strings.Split(addrStr, "/")
	if len(parts) != 3 {
		err = fmt.Errorf("Error parsing addrStr: %s", addrStr)
		return
	}

	host = parts[0]

	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return
	}

	useTLSInt, err := strconv.Atoi(parts[2])
	if err != nil {
		return
	}

	if useTLSInt == 0 {
		useTLS = false
	} else {
		useTLS = true
	}

	return
}

func (a *proxyAddr) Network() string {
	return EncodeRemoteAddr(a.Host, a.Port, a.UseTLS)
}

func (a *proxyAddr) String() string {
	return EncodeRemoteAddr(a.Host, a.Port, a.UseTLS)
}

//// bufferedConn and wrappers
type bufferedConn struct {
	reader   *bufio.Reader
	net.Conn // Embed conn
}

func (c bufferedConn) Peek(n int) ([]byte, error) {
	return c.reader.Peek(n)
}

func (c bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

//// Implement net.Conn
func (c *proxyConn) Read(b []byte) (n int, err error) {
	if c.readReq != nil {
		buf := new(bytes.Buffer)
		c.readReq.Write(buf)
		s := buf.String()
		n = 0
		for n = 0; n < len(b) && n < len(s); n++ {
			b[n] = s[n]
		}
		c.readReq = nil
		return n, nil
	}
	if c.conn == nil {
		return 0, fmt.Errorf("ProxyConn %d does not have an active connection", c.Id())
	}
	return c.conn.Read(b)
}

func (c *proxyConn) Write(b []byte) (n int, err error) {
	return c.conn.Write(b)
}

func (c *proxyConn) Close() error {
	return c.conn.Close()
}

func (c *proxyConn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *proxyConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *proxyConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (c *proxyConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *proxyConn) RemoteAddr() net.Addr {
	// RemoteAddr encodes the destination server for this connection
	return c.Addr
}

//// Implement ProxyConn
func (pconn *proxyConn) Id() int {
	return pconn.id
}

func (pconn *proxyConn) Logger() *log.Logger {
	return pconn.logger
}

func (pconn *proxyConn) SetCACertificate(cert *tls.Certificate) {
	pconn.caCert = cert
}

func (pconn *proxyConn) StartMaybeTLS(hostname string) (bool, error) {
	// Prepares to start doing TLS if the client starts. Returns whether TLS was started

	// Wrap the ProxyConn's net.Conn in a bufferedConn
	bufConn := bufferedConn{bufio.NewReader(pconn.conn), pconn.conn}
	usingTLS := false

	// Guess if we're doing TLS
	byte, err := bufConn.Peek(1)
	if err != nil {
		return false, err
	}
	if byte[0] == '\x16' {
		usingTLS = true
	}

	if usingTLS {
		if err != nil {
			return false, err
		}

		cert, err := SignHost(*pconn.caCert, []string{hostname})
		if err != nil {
			return false, err
		}

		config := &tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{cert},
		}
		tlsConn := tls.Server(bufConn, config)
		pconn.conn = tlsConn
		return true, nil
	} else {
		pconn.conn = bufConn
		return false, nil
	}
}

func NewProxyConn(c net.Conn, l *log.Logger) *proxyConn {
	a := proxyAddr{Host: "", Port: -1, UseTLS: false}
	p := proxyConn{Addr: &a, logger: l, conn: c, readReq: nil}
	p.id = getNextConnId()
	return &p
}

func (pconn *proxyConn) returnRequest(req *http.Request) {
	pconn.readReq = req
}

/*
Implements net.Listener. Listeners can be added. Will accept
connections on each listener and read HTTP messages from the
connection. Will attempt to spoof TLS from incoming HTTP
requests. Accept() returns a ProxyConn which transmists one
unencrypted HTTP request and contains the intended destination for
each request.
*/
type ProxyListener struct {
	net.Listener

	State int

	inputListeners mapset.Set
	mtx            sync.Mutex
	logger         *log.Logger
	outputConns    chan ProxyConn
	inputConns     chan net.Conn
	outputConnDone chan struct{}
	inputConnDone  chan struct{}
	listenWg       sync.WaitGroup
	caCert         *tls.Certificate
}

type listenerData struct {
	Id       int
	Listener net.Listener
}

func newListenerData(listener net.Listener) *listenerData {
	l := listenerData{}
	l.Id = getNextListenerId()
	l.Listener = listener
	return &l
}

func NewProxyListener(logger *log.Logger) *ProxyListener {
	l := ProxyListener{logger: logger, State: ProxyStarting}
	l.inputListeners = mapset.NewSet()

	l.outputConns = make(chan ProxyConn)
	l.inputConns = make(chan net.Conn)
	l.outputConnDone = make(chan struct{})
	l.inputConnDone = make(chan struct{})

	// Translate connections
	l.listenWg.Add(1)
	go func() {
		l.logger.Println("Starting connection translator...")
		defer l.listenWg.Done()
		for {
			select {
			case <-l.outputConnDone:
				l.logger.Println("Output channel closed. Shutting down translator.")
				return
			case inconn := <-l.inputConns:
				go func() {
					err := l.translateConn(inconn)
					if err != nil {
						l.logger.Println("Could not translate connection:", err)
					}
				}()
			}
		}
	}()

	l.State = ProxyRunning
	l.logger.Println("Proxy Started")

	return &l
}

func (listener *ProxyListener) Accept() (net.Conn, error) {
	if listener.outputConns == nil ||
		listener.inputConns == nil ||
		listener.outputConnDone == nil ||
		listener.inputConnDone == nil {
		return nil, fmt.Errorf("Listener not initialized! Cannot accept connection.")

	}
	select {
	case <-listener.outputConnDone:
		listener.logger.Println("Cannot accept connection, ProxyListener is closed")
		return nil, fmt.Errorf("Connection is closed")
	case c := <-listener.outputConns:
		listener.logger.Println("Connection", c.Id(), "accepted from ProxyListener")
		return c, nil
	}
}

func (listener *ProxyListener) Close() error {
	listener.mtx.Lock()
	defer listener.mtx.Unlock()

	listener.logger.Println("Closing ProxyListener...")
	listener.State = ProxyStopped
	close(listener.outputConnDone)
	close(listener.inputConnDone)
	close(listener.outputConns)
	close(listener.inputConns)

	it := listener.inputListeners.Iterator()
	for elem := range it.C {
		l := elem.(*listenerData)
		l.Listener.Close()
		listener.logger.Println("Closed listener", l.Id)
	}
	listener.logger.Println("ProxyListener closed")
	listener.listenWg.Wait()
	return nil
}

func (listener *ProxyListener) Addr() net.Addr {
	return InternalAddr{}
}

// Add a listener for the ProxyListener to listen on
func (listener *ProxyListener) AddListener(inlisten net.Listener) error {
	listener.mtx.Lock()
	defer listener.mtx.Unlock()

	listener.logger.Println("Adding listener to ProxyListener:", inlisten)
	il := newListenerData(inlisten)
	l := listener
	listener.listenWg.Add(1)
	go func() {
		defer l.listenWg.Done()
		for {
			c, err := il.Listener.Accept()
			if err != nil {
				// TODO: verify that the connection is actually closed and not some other error
				l.logger.Println("Listener", il.Id, "closed")
				return
			}
			l.logger.Println("Received conn form listener", il.Id)
			l.inputConns <- c
		}
	}()
	listener.inputListeners.Add(il)
	l.logger.Println("Listener", il.Id, "added to ProxyListener")
	return nil
}

// Close a listener and remove it from the slistener. Does not kill active connections.
func (listener *ProxyListener) RemoveListener(inlisten net.Listener) error {
	listener.mtx.Lock()
	defer listener.mtx.Unlock()

	listener.inputListeners.Remove(inlisten)
	inlisten.Close()
	listener.logger.Println("Listener removed:", inlisten)
	return nil
}

// Take in a connection, strip TLS, get destination info, and push a ProxyConn to the listener.outputConnection channel
func (listener *ProxyListener) translateConn(inconn net.Conn) error {
	pconn := NewProxyConn(inconn, listener.logger)
	pconn.SetCACertificate(listener.GetCACertificate())

	var host string = ""
	var port int = -1
	var useTLS bool = false

	request, err := http.ReadRequest(bufio.NewReader(pconn))
	if err != nil {
		listener.logger.Println(err)
		return err
	}

	// Get parsed host and port
	parsed_host, sport, err := net.SplitHostPort(request.URL.Host)
	if err != nil {
		// Assume that that URL.Host is the hostname and doesn't contain a port
		host = request.URL.Host
		port = -1
	} else {
		parsed_port, err := strconv.Atoi(sport)
		if err != nil {
			// Assume that that URL.Host is the hostname and doesn't contain a port
			return fmt.Errorf("Error parsing hostname: %s", err)
		}
		host = parsed_host
		port = parsed_port
	}

	// Handle CONNECT and TLS
	if request.Method == "CONNECT" {
		// Respond that we connected
		resp := http.Response{Status: "Connection established", Proto: "HTTP/1.1", ProtoMajor: 1, StatusCode: 200}
		err := resp.Write(inconn)
		if err != nil {
			listener.logger.Println("Could not write CONNECT response:", err)
			return err
		}

		usedTLS, err := pconn.StartMaybeTLS(host)
		if err != nil {
			listener.logger.Println("Error starting maybeTLS:", err)
			return err
		}
		useTLS = usedTLS
	} else {
		// Put the request back
		pconn.returnRequest(request)
		useTLS = false
	}

	// Guess the port if we have to
	if port == -1 {
		if useTLS {
			port = 443
		} else {
			port = 80
		}
	}
	pconn.Addr.Host = host
	pconn.Addr.Port = port
	pconn.Addr.UseTLS = useTLS
	var useTLSStr string
	if pconn.Addr.UseTLS {
		useTLSStr = "YES"
	} else {
		useTLSStr = "NO"
	}
	pconn.Logger().Printf("Received connection to: Host='%s', Port=%d, UseTls=%s", pconn.Addr.Host, pconn.Addr.Port, useTLSStr)

	// Put the conn in the output channel
	listener.outputConns <- pconn
	return nil
}

func (listener *ProxyListener) SetCACertificate(caCert *tls.Certificate) {
	listener.mtx.Lock()
	defer listener.mtx.Unlock()

	listener.caCert = caCert
}

func (listener *ProxyListener) GetCACertificate() *tls.Certificate {
	listener.mtx.Lock()
	defer listener.mtx.Unlock()

	return listener.caCert
}
