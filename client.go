package coap

// A client implementation.

import (
	"crypto/tls"
	"log"
	"net"
	"strings"
	"time"
)

// A ClientConn represents a connection to a COAP server.
type ClientConn struct {
	srv          *Server
	client       *Client
	session      SessionNet
	shutdownSync chan error
}

// A Client defines parameters for a COAP client.
type Client struct {
	Net            string        // if "tcp" or "tcp-tls" (COAP over TLS) a TCP query will be initiated, otherwise an UDP one (default is "" for UDP) or "udp-mcast" for multicast
	MaxMessageSize uint16        // Max message size that could be received from peer. If not set it defaults to 1152 B.
	TLSConfig      *tls.Config   // TLS connection configuration
	DialTimeout    time.Duration // set Timeout for dialer
	ReadTimeout    time.Duration // net.ClientConn.SetReadTimeout value for connections, defaults to 1 hour - overridden by Timeout when that value is non-zero
	WriteTimeout   time.Duration // net.ClientConn.SetWriteTimeout value for connections, defaults to 1 hour - overridden by Timeout when that value is non-zero
	SyncTimeout    time.Duration // The maximum of time for synchronization go-routines, defaults to 30 seconds - overridden by Timeout when that value is non-zero if it occurs, then it call log.Fatal

	ObserverFunc         HandlerFunc     // for handling observation messages from server
	NotifySessionEndFunc func(err error) // if NotifySessionEndFunc is set it is called when TCP/UDP session was ended.

	BlockWiseTransfer    *bool     // Use blockWise transfer for transfer payload (default for UDP it's enabled, for TCP it's disable)
	BlockWiseTransferSzx *BlockSzx // Set maximal block size of payload that will be send in fragment
}

func (c *Client) readTimeout() time.Duration {
	if c.ReadTimeout != 0 {
		return c.ReadTimeout
	}
	return coapTimeout
}

func (c *Client) writeTimeout() time.Duration {
	if c.WriteTimeout != 0 {
		return c.WriteTimeout
	}
	return coapTimeout
}

func (c *Client) syncTimeout() time.Duration {
	if c.SyncTimeout != 0 {
		return c.SyncTimeout
	}
	return syncTimeout
}

// Dial connects to the address on the named network.
func (c *Client) Dial(address string) (clientConn *ClientConn, err error) {

	var conn net.Conn
	var network string
	var sessionUPDData *SessionUDPData

	dialer := &net.Dialer{Timeout: c.DialTimeout}
	BlockWiseTransfer := false
	BlockWiseTransferSzx := BlockSzx1024

	switch c.Net {
	case "tcp-tls", "tcp4-tls", "tcp6-tls":
		network = strings.TrimSuffix(c.Net, "-tls")
		conn, err = tls.DialWithDialer(dialer, network, address, c.TLSConfig)
		if err != nil {
			return nil, err
		}
		BlockWiseTransferSzx = BlockSzxBERT
	case "tcp", "tcp4", "tcp6":
		network = c.Net
		conn, err = dialer.Dial(c.Net, address)
		if err != nil {
			return nil, err
		}
		BlockWiseTransferSzx = BlockSzxBERT
	case "udp", "udp4", "udp6", "":
		network = c.Net
		if network == "" {
			network = "udp"
		}
		if conn, err = dialer.Dial(network, address); err != nil {
			return nil, err
		}
		sessionUPDData = &SessionUDPData{raddr: conn.(*net.UDPConn).RemoteAddr().(*net.UDPAddr)}
		BlockWiseTransfer = true
	case "udp-mcast", "udp4-mcast", "udp6-mcast":
		network = strings.TrimSuffix(c.Net, "-mcast")
		var a *net.UDPAddr
		if a, err = net.ResolveUDPAddr(network, address); err != nil {
			return nil, err
		}
		var udpConn *net.UDPConn
		if udpConn, err = net.ListenUDP(network, a); err != nil {
			return nil, err
		}
		if err := setUDPSocketOptions(udpConn); err != nil {
			return nil, err
		}
		sessionUPDData = &SessionUDPData{raddr: a}
		conn = udpConn
		BlockWiseTransfer = true
	default:
		return nil, ErrInvalidNetParameter
	}

	if c.BlockWiseTransfer != nil {
		BlockWiseTransfer = *c.BlockWiseTransfer
	}

	if c.BlockWiseTransferSzx != nil {
		BlockWiseTransferSzx = *c.BlockWiseTransferSzx
	}

	sync := make(chan bool)
	clientConn = &ClientConn{srv: &Server{
		Net:                  network,
		TLSConfig:            c.TLSConfig,
		Conn:                 conn,
		ReadTimeout:          c.readTimeout(),
		WriteTimeout:         c.writeTimeout(),
		MaxMessageSize:       c.MaxMessageSize,
		BlockWiseTransfer:    &BlockWiseTransfer,
		BlockWiseTransferSzx: &BlockWiseTransferSzx,
		NotifyStartedFunc: func() {
			timeout := c.syncTimeout()
			select {
			case sync <- true:
			case <-time.After(timeout):
				log.Fatal("Client cannot send start: Timeout")
			}
		},
		NotifySessionEndFunc: func(s SessionNet, err error) {
			if c.NotifySessionEndFunc != nil {
				c.NotifySessionEndFunc(err)
			}
		},
		createSessionTCPFunc: func(connection Conn, srv *Server) (SessionNet, error) {
			return clientConn.session, nil
		},
		createSessionUDPFunc: func(connection Conn, srv *Server, sessionUDPData *SessionUDPData) (SessionNet, error) {
			if sessionUDPData.RemoteAddr().String() == clientConn.session.RemoteAddr().String() {
				if s, ok := clientConn.session.(*blockWiseSession); ok {
					s.SessionNet.(*sessionUDP).sessionUDPData = sessionUDPData
				} else {
					clientConn.session.(*sessionUDP).sessionUDPData = sessionUDPData
				}
				return clientConn.session, nil
			}
			session, err := newSessionUDP(connection, srv, sessionUDPData)
			if err != nil {
				return nil, err
			}
			if session.blockWiseEnabled() {
				return &blockWiseSession{SessionNet: session}, nil
			}
			return session, nil
		}, Handler: c.ObserverFunc},
		shutdownSync: make(chan error)}

	switch clientConn.srv.Conn.(type) {
	case *net.TCPConn, *tls.Conn:
		session, err := newSessionTCP(newConnectionTCP(clientConn.srv.Conn, clientConn.srv), clientConn.srv)
		if err != nil {
			return nil, err
		}
		if session.blockWiseEnabled() {
			clientConn.session = &blockWiseSession{SessionNet: session}
		} else {
			clientConn.session = session
		}
	case *net.UDPConn:
		// WriteMsgUDP returns error when addr is filled in SessionUDPData for connected socket
		setUDPSocketOptions(clientConn.srv.Conn.(*net.UDPConn))
		session, err := newSessionUDP(newConnectionUDP(clientConn.srv.Conn.(*net.UDPConn), clientConn.srv), clientConn.srv, sessionUPDData)
		if err != nil {
			return nil, err
		}
		if session.blockWiseEnabled() {
			clientConn.session = &blockWiseSession{SessionNet: session}
		} else {
			clientConn.session = session
		}
	}

	clientConn.session.SetReadDeadline(c.readTimeout())
	clientConn.session.SetWriteDeadline(c.writeTimeout())

	go func() {
		timeout := c.syncTimeout()
		err := clientConn.srv.ActivateAndServe()
		select {
		case clientConn.shutdownSync <- err:
		case <-time.After(timeout):
			log.Fatal("Client cannot send shutdown: Timeout")
		}
	}()

	select {
	case <-sync:
	case <-time.After(c.syncTimeout()):
		log.Fatal("Client cannot recv start: Timeout")
	}

	clientConn.client = c

	return clientConn, nil
}

// LocalAddr implements the SessionNet.LocalAddr method.
func (co *ClientConn) LocalAddr() net.Addr {
	return co.session.LocalAddr()
}

// RemoteAddr implements the SessionNet.RemoteAddr method.
func (co *ClientConn) RemoteAddr() net.Addr {
	return co.session.RemoteAddr()
}

// Exchange performs a synchronous query. It sends the message m to the address
// contained in a and waits for a reply.
//
// Exchange does not retry a failed query, nor will it fall back to TCP in
// case of truncation.
// To specify a local address or a timeout, the caller has to set the `Client.Dialer`
// attribute appropriately
func (co *ClientConn) Exchange(m Message) (Message, error) {
	return co.session.Exchange(m)
}

// NewMessage Create message for request
func (co *ClientConn) NewMessage(p MessageParams) Message {
	return co.session.NewMessage(p)
}

// WriteMsg sends a message through the connection co.
func (co *ClientConn) Write(m Message) error {
	return co.session.Write(m)
}

// SetReadDeadline set read deadline for timeout for Exchange
func (co *ClientConn) SetReadDeadline(timeout time.Duration) {
	co.session.SetReadDeadline(timeout)
}

// SetWriteDeadline set write deadline for timeout for Exchange and Write
func (co *ClientConn) SetWriteDeadline(timeout time.Duration) {
	co.session.SetWriteDeadline(timeout)
}

// Ping send a ping message and wait for a pong response
func (co *ClientConn) Ping(timeout time.Duration) error {
	return co.session.Ping(timeout)
}

// Close close connection
func (co *ClientConn) Close() {
	co.srv.Shutdown()
	select {
	case <-co.shutdownSync:
	case <-time.After(co.client.syncTimeout()):
		log.Fatal("Client cannot recv shutdown: Timeout")
	}
}

// Dial connects to the address on the named network.
func Dial(network, address string) (*ClientConn, error) {
	client := Client{Net: network}
	return client.Dial(address)
}

// DialTimeout acts like Dial but takes a timeout.
func DialTimeout(network, address string, timeout time.Duration) (*ClientConn, error) {
	client := Client{Net: network, DialTimeout: timeout}
	return client.Dial(address)
}

func fixNetTLS(network string) string {
	if !strings.HasSuffix(network, "-tls") {
		network += "-tls"
	}
	return network
}

// DialWithTLS connects to the address on the named network with TLS.
func DialWithTLS(network, address string, tlsConfig *tls.Config) (conn *ClientConn, err error) {
	client := Client{Net: fixNetTLS(network), TLSConfig: tlsConfig}
	return client.Dial(address)
}

// DialTimeoutWithTLS acts like DialWithTLS but takes a timeout.
func DialTimeoutWithTLS(network, address string, tlsConfig *tls.Config, timeout time.Duration) (conn *ClientConn, err error) {
	client := Client{Net: fixNetTLS(network), DialTimeout: timeout, TLSConfig: tlsConfig}
	return client.Dial(address)
}
