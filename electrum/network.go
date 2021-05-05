package electrum

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// ClientVersion identifies the client version/name to the remote server
	ClientVersion = "go-electrum1.0"

	// ProtocolVersion identifies the support protocol version to the remote server
	ProtocolVersion = "1.4"

	nl = byte('\n')
)

var (
	// DebugMode provides debug output on communications with the remote server if enabled.
	DebugMode bool

	// ErrServerConnected throws an error if remote server is already connected.
	ErrServerConnected = errors.New("server is already connected")

	// ErrServerShutdown throws an error if remote server has shutdown.
	ErrServerShutdown = errors.New("server has shutdown")

	// ErrTimeout throws an error if request has timed out
	ErrTimeout = errors.New("request timeout")

	// ErrNotImplemented throws an error if this RPC call has not been implemented yet.
	ErrNotImplemented = errors.New("RPC call is not implemented")

	// ErrDeprecated throws an error if this RPC call is deprecated.
	ErrDeprecated = errors.New("RPC call has been deprecated")

	DefaultServerOptions = ServerOptions{
		ConnTimeout: 30 * time.Second,
		ReqTimeout:  30 * time.Second,
	}
)

// Transport provides interface to server transport.
type Transport interface {
	SendMessage([]byte) error
	Responses() <-chan []byte
	Errors() <-chan error
	Close() error
}

// TCPTransport store information about the TCP transport.
type TCPTransport struct {
	conn      net.Conn
	responses chan []byte
	errors    chan error
}

// NewTCPTransport opens a new TCP connection to the remote server.
func NewTCPTransport(addr string, timeout time.Duration) (*TCPTransport, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}

	tcp := &TCPTransport{
		conn:      conn,
		responses: make(chan []byte),
		errors:    make(chan error),
	}

	go tcp.listen()

	return tcp, nil
}

// NewSSLTransport opens a new SSL connection to the remote server.
func NewSSLTransport(addr string, config *tls.Config, timeout time.Duration) (*TCPTransport, error) {
	dialer := net.Dialer{
		Timeout: timeout,
	}
	conn, err := tls.DialWithDialer(&dialer, "tcp", addr, config)
	if err != nil {
		return nil, err
	}

	tcp := &TCPTransport{
		conn:      conn,
		responses: make(chan []byte),
		errors:    make(chan error),
	}

	go tcp.listen()

	return tcp, nil
}

func (t *TCPTransport) listen() {
	defer t.conn.Close()
	reader := bufio.NewReader(t.conn)

	for {
		line, err := reader.ReadBytes(nl)
		if err != nil {
			t.errors <- err
			break
		}
		if DebugMode {
			log.Printf("%s [debug] %s -> %s", time.Now().Format("2006-01-02 15:04:05"), t.conn.RemoteAddr(), line)
		}

		t.responses <- line
	}
}

// SendMessage sends a message to the remote server through the TCP transport.
func (t *TCPTransport) SendMessage(body []byte) error {
	if DebugMode {
		log.Printf("%s [debug] %s <- %s", time.Now().Format("2006-01-02 15:04:05"), t.conn.RemoteAddr(), body)
	}

	_, err := t.conn.Write(body)
	return err
}

// Responses returns chan to TCP transport responses.
func (t *TCPTransport) Responses() <-chan []byte {
	return t.responses
}

// Errors returns chan to TCP transport errors.
func (t *TCPTransport) Errors() <-chan error {
	return t.errors
}

func (t *TCPTransport) Close() error {
	return t.conn.Close()
}

type container struct {
	content []byte
	err     error
}

type ServerOptions struct {
	ConnTimeout time.Duration
	ReqTimeout  time.Duration
}

// Server stores information about the remote server.
type Server struct {
	transport Transport
	opts      *ServerOptions

	handlers     map[uint64]chan *container
	handlersLock sync.RWMutex

	pushHandlers     map[string][]chan *container
	pushHandlersLock sync.RWMutex

	Error chan error
	quit  chan struct{}

	nextID uint64
}

// NewServer initialize a new remote server.
func NewServer(opts *ServerOptions) *Server {
	s := &Server{
		handlers:     make(map[uint64]chan *container),
		pushHandlers: make(map[string][]chan *container),

		Error: make(chan error),
		quit:  make(chan struct{}),

		opts: opts,
	}

	return s
}

// ConnectTCP connects to the remote server using TCP.
func (s *Server) ConnectTCP(addr string) error {
	if s.transport != nil {
		return ErrServerConnected
	}

	transport, err := NewTCPTransport(addr, s.opts.ConnTimeout)
	if err != nil {
		return err
	}

	s.transport = transport
	go s.listen()

	return nil
}

// ConnectSSL connects to the remote server using SSL.
func (s *Server) ConnectSSL(addr string, config *tls.Config) error {
	if s.transport != nil {
		return ErrServerConnected
	}

	transport, err := NewSSLTransport(addr, config, s.opts.ConnTimeout)
	if err != nil {
		return err
	}

	s.transport = transport
	go s.listen()

	return nil
}

type apiErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *apiErr) Error() string {
	return fmt.Sprintf("errNo: %d, errMsg: %s", e.Code, e.Message)
}

type response struct {
	ID     uint64  `json:"id"`
	Method string  `json:"method"`
	Error  *apiErr `json:"error"`
}

func (s *Server) listen() {
	for {
		if s.IsShutdown() {
			break
		}
		if s.transport == nil {
			break
		}
		select {
		case <-s.quit:
			return
		case err := <-s.transport.Errors():
			s.Error <- err
			s.Shutdown()
		case bytes := <-s.transport.Responses():
			result := &container{
				content: bytes,
			}

			msg := &response{}
			err := json.Unmarshal(bytes, msg)
			if err != nil {
				if DebugMode {
					log.Printf("Unmarshal received message failed: %v", err)
				}
				result.err = fmt.Errorf("unmarshal received message failed: %v", err)
			} else if msg.Error != nil {
				result.err = msg.Error
			}

			if len(msg.Method) > 0 {
				s.pushHandlersLock.RLock()
				handlers := s.pushHandlers[msg.Method]
				s.pushHandlersLock.RUnlock()

				for _, handler := range handlers {
					select {
					case handler <- result:
					default:
					}
				}
			}

			s.handlersLock.RLock()
			c, ok := s.handlers[msg.ID]
			s.handlersLock.RUnlock()

			if ok {
				c <- result
			}
		}
	}
}

func (s *Server) listenPush(method string) <-chan *container {
	c := make(chan *container, 1)
	s.pushHandlersLock.Lock()
	s.pushHandlers[method] = append(s.pushHandlers[method], c)
	s.pushHandlersLock.Unlock()

	return c
}

type request struct {
	ID     uint64        `json:"id"`
	Method string        `json:"method"`
	Params []interface{} `json:"params"`
}

func (s *Server) request(method string, params []interface{}, v interface{}) error {
	select {
	case <-s.quit:
		return ErrServerShutdown
	default:
	}

	msg := request{
		ID:     atomic.AddUint64(&s.nextID, 1),
		Method: method,
		Params: params,
	}

	bytes, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	bytes = append(bytes, nl)

	err = s.transport.SendMessage(bytes)
	if err != nil {
		s.Shutdown()
		return err
	}

	c := make(chan *container, 1)

	s.handlersLock.Lock()
	s.handlers[msg.ID] = c
	s.handlersLock.Unlock()

	var resp *container
	select {
	case resp = <-c:
	case <-time.After(s.opts.ReqTimeout):
		return ErrTimeout
	}

	if resp.err != nil {
		return resp.err
	}

	s.handlersLock.Lock()
	delete(s.handlers, msg.ID)
	s.handlersLock.Unlock()

	if v != nil {
		err = json.Unmarshal(resp.content, v)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) Shutdown() {
	if !s.IsShutdown() {
		close(s.quit)
	}
	if s.transport != nil {
		_ = s.transport.Close()
	}
	s.transport = nil
	s.handlers = nil
	s.pushHandlers = nil
}

func (s *Server) IsShutdown() bool {
	select {
	case <-s.quit:
		return true
	default:
	}
	return false
}
