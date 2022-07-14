// Package recws provides websocket client based on gorilla/websocket
// that will automatically reconnect if the connection is dropped.
package recws

import (
	"crypto/tls"
	"errors"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
)

// ErrNotConnected is returned when the application read/writes
// a message and the connection is closed
var ErrNotConnected = errors.New("websocket: not connected")

// The RecConn type represents a Reconnecting WebSocket connection.
type RecConn struct {
	// RecIntvlMin specifies the initial reconnecting interval,
	// default to 2 seconds
	RecIntvlMin time.Duration
	// RecIntvlMax specifies the maximum reconnecting interval,
	// default to 30 seconds
	RecIntvlMax time.Duration
	// RecIntvlFactor specifies the rate of increase of the reconnection
	// interval, default to 1.5
	RecIntvlFactor float64
	// HandshakeTimeout specifies the duration for the handshake to complete,
	// default to 2 seconds
	HandshakeTimeout time.Duration
	// Proxy specifies the proxy function for the dialer
	// defaults to ProxyFromEnvironment
	Proxy func(*http.Request) (*url.URL, error)
	// Client TLS config to use on reconnect
	TLSClientConfig *tls.Config
	// SubscribeHandler fires after the connection successfully establish.
	SubscribeHandler func() error
	// DisconnectHandler fires after the connection has been closed
	DisconnectHandler func()
	// PongHandler fires on every Pong control message received
	PongHandler func() error
	// KeepAliveTimeout is an interval for sending ping/pong messages
	// disabled if 0
	KeepAliveTimeout time.Duration
	// keepAliveDone is a channel which triggers the closing of all
	// previous monitoring goroutines
	keepAliveDone chan struct{}
	// NonVerbose suppress connecting/reconnecting messages.
	NonVerbose bool

	isConnected    bool
	isReconnecting bool
	mu             sync.RWMutex
	url            string
	reqHeader      http.Header
	httpResp       *http.Response
	dialErr        error
	dialer         *websocket.Dialer

	*websocket.Conn
}

var keepAliveCounter int64

// CloseAndReconnect will try to reconnect.
func (rc *RecConn) CloseAndReconnect() {
	if rc.IsReconnecting() {
		return
	}

	rc.setIsReconnecting(true)
	rc.Close()
	go rc.connect()
}

// setIsConnected sets state for isConnected
func (rc *RecConn) setIsConnected(state bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.isConnected = state
}

func (rc *RecConn) getConn() *websocket.Conn {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.Conn
}

// Close closes the underlying network connection without
// sending or waiting for a close frame.
func (rc *RecConn) Close() {
	if rc.getConn() != nil {
		rc.mu.Lock()
		rc.Conn.Close()
		rc.mu.Unlock()
	}

	rc.setIsConnected(false)

	if rc.hasDisconnectHandler() {
		rc.DisconnectHandler()
	}
}

// Shutdown gracefully closes the connection by sending the websocket.CloseMessage.
// The writeWait param defines the duration before the deadline of the write operation is hit.
func (rc *RecConn) Shutdown(writeWait time.Duration) {
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	err := rc.WriteControl(websocket.CloseMessage, msg, time.Now().Add(writeWait))
	if err != nil && err != websocket.ErrCloseSent {
		// If close message could not be sent, then close without the handshake.
		log.Printf("Shutdown: %v", err)
		rc.Close()
	}
}

// ReadMessage is a helper method for getting a reader
// using NextReader and reading from that reader to a buffer.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) ReadMessage() (messageType int, message []byte, err error) {
	err = ErrNotConnected
	if rc.IsConnected() {
		messageType, message, err = rc.Conn.ReadMessage()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return messageType, message, nil
		}
		if err != nil {
			rc.CloseAndReconnect()
		}
	}

	return
}

// WriteMessage is a helper method for getting a writer using NextWriter,
// writing the message and closing the writer.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) WriteMessage(messageType int, data []byte) error {
	err := ErrNotConnected
	if rc.IsConnected() {
		rc.mu.Lock()
		err = rc.Conn.WriteMessage(messageType, data)
		rc.mu.Unlock()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil {
			rc.CloseAndReconnect()
		}
	}

	return err
}

// WriteJSON writes the JSON encoding of v to the connection.
//
// See the documentation for encoding/json Marshal for details about the
// conversion of Go values to JSON.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) WriteJSON(v interface{}) error {
	err := ErrNotConnected
	if rc.IsConnected() {
		rc.mu.Lock()
		err = rc.Conn.WriteJSON(v)
		rc.mu.Unlock()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil {
			rc.CloseAndReconnect()
		}
	}

	return err
}

// ReadJSON reads the next JSON-encoded message from the connection and stores
// it in the value pointed to by v.
//
// See the documentation for the encoding/json Unmarshal function for details
// about the conversion of JSON to a Go value.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) ReadJSON(v interface{}) error {
	err := ErrNotConnected
	if rc.IsConnected() {
		err = rc.Conn.ReadJSON(v)
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil {
			rc.CloseAndReconnect()
		}
	}

	return err
}

func (rc *RecConn) setURL(url string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.url = url
}

func (rc *RecConn) setReqHeader(reqHeader http.Header) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.reqHeader = reqHeader
}

// parseURL parses current url
func (rc *RecConn) parseURL(urlStr string) (string, error) {
	if urlStr == "" {
		return "", errors.New("dial: url cannot be empty")
	}

	u, err := url.Parse(urlStr)

	if err != nil {
		return "", errors.New("url: " + err.Error())
	}

	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", errors.New("url: websocket uris must start with ws or wss scheme")
	}

	if u.User != nil {
		return "", errors.New("url: user name and password are not allowed in websocket URIs")
	}

	return urlStr, nil
}

func (rc *RecConn) setDefaultRecIntvlMin() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.RecIntvlMin <= 0 {
		rc.RecIntvlMin = 2 * time.Second
	}
}

func (rc *RecConn) setDefaultRecIntvlMax() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.RecIntvlMax <= 0 {
		rc.RecIntvlMax = 30 * time.Second
	}
}

func (rc *RecConn) setDefaultRecIntvlFactor() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.RecIntvlFactor <= 0 {
		rc.RecIntvlFactor = 1.5
	}
}

func (rc *RecConn) setDefaultHandshakeTimeout() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.HandshakeTimeout <= 0 {
		rc.HandshakeTimeout = 2 * time.Second
	}
}

func (rc *RecConn) setDefaultProxy() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.Proxy == nil {
		rc.Proxy = http.ProxyFromEnvironment
	}
}

func (rc *RecConn) setDefaultDialer(tlsClientConfig *tls.Config, handshakeTimeout time.Duration) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.dialer = &websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		Proxy:            rc.Proxy,
		TLSClientConfig:  tlsClientConfig,
	}
}

func (rc *RecConn) getHandshakeTimeout() time.Duration {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.HandshakeTimeout
}

func (rc *RecConn) getTLSClientConfig() *tls.Config {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.TLSClientConfig
}

func (rc *RecConn) SetTLSClientConfig(tlsClientConfig *tls.Config) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.TLSClientConfig = tlsClientConfig
}

// Dial creates a new client connection.
// The URL url specifies the host and request URI. Use requestHeader to specify
// the origin (Origin), subprotocols (Sec-WebSocket-Protocol) and cookies
// (Cookie). Use GetHTTPResponse() method for the response.Header to get
// the selected subprotocol (Sec-WebSocket-Protocol) and cookies (Set-Cookie).
func (rc *RecConn) Dial(urlStr string, reqHeader http.Header) {
	urlStr, err := rc.parseURL(urlStr)

	if err != nil {
		log.Fatalf("Dial: %v", err)
	}

	// Config
	rc.setURL(urlStr)
	rc.setReqHeader(reqHeader)
	rc.setDefaultRecIntvlMin()
	rc.setDefaultRecIntvlMax()
	rc.setDefaultRecIntvlFactor()
	rc.setDefaultHandshakeTimeout()
	rc.setDefaultProxy()
	rc.setDefaultDialer(rc.getTLSClientConfig(), rc.getHandshakeTimeout())

	// Connect
	go rc.connect()

	// wait on first attempt
	time.Sleep(rc.getHandshakeTimeout())
}

// GetURL returns current connection url
func (rc *RecConn) GetURL() string {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.url
}

func (rc *RecConn) getNonVerbose() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.NonVerbose
}

func (rc *RecConn) getBackoff() *backoff.Backoff {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return &backoff.Backoff{
		Min:    rc.RecIntvlMin,
		Max:    rc.RecIntvlMax,
		Factor: rc.RecIntvlFactor,
		Jitter: true,
	}
}

func (rc *RecConn) hasSubscribeHandler() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.SubscribeHandler != nil
}

func (rc *RecConn) hasDisconnectHandler() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.DisconnectHandler != nil
}

func (rc *RecConn) hasPongHandler() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.PongHandler != nil
}

func (rc *RecConn) getKeepAliveTimeout() time.Duration {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.KeepAliveTimeout
}

func (rc *RecConn) resetKeepAliveDone() chan struct{} {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	if rc.keepAliveDone != nil {
		close(rc.keepAliveDone)
	}

	rc.keepAliveDone = make(chan struct{})

	return rc.keepAliveDone
}

func (rc *RecConn) writeControlPingMessage(writeWait time.Duration) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	return rc.Conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(writeWait))
}

func (rc *RecConn) keepAlive() {
	var (
		keepAliveResponse = new(keepAliveResponse)
		keepAliveTimeout  = rc.getKeepAliveTimeout()
		keepAliveDone     = rc.resetKeepAliveDone()
		keepAliveId       = atomic.AddInt64(&keepAliveCounter, 1)
		ticker            = time.NewTicker(keepAliveTimeout)
		pingTicker        = time.NewTicker(time.Second)
		pingSent          = false
	)

	if !rc.getNonVerbose() {
		log.Printf("WS: started Keep Alive #%d\n", keepAliveId)
	}

	rc.mu.Lock()
	rc.Conn.SetPongHandler(func(msg string) error {
		keepAliveResponse.setLastResponse()

		if rc.hasPongHandler() {
			return rc.PongHandler()
		}

		return nil
	})
	rc.mu.Unlock()

	go func() {
		defer ticker.Stop()
		defer pingTicker.Stop()

		for {
			select {
			case <-keepAliveDone:
				log.Printf("WS: closed Keep Alive #%d\n", keepAliveId)
				return
			case <-pingTicker.C:
				if !pingSent && rc.IsConnected() {
					if err := rc.writeControlPingMessage(keepAliveTimeout); err != nil {
						log.Println(err)
					}
					pingSent = true
				}
			case <-ticker.C:
				if time.Since(keepAliveResponse.getLastResponse()) > keepAliveTimeout {
					rc.CloseAndReconnect()
					if !rc.getNonVerbose() {
						log.Printf("WS: closed Keep Alive #%d\n", keepAliveId)
					}
					return
				}
				pingSent = false
			}
		}
	}()
}

func (rc *RecConn) connect() {
	b := rc.getBackoff()
	rand.Seed(time.Now().UTC().UnixNano())

	for {
		nextItvl := b.Duration()
		wsConn, httpResp, err := rc.dialer.Dial(rc.url, rc.reqHeader)

		rc.mu.Lock()
		rc.Conn = wsConn
		rc.dialErr = err
		rc.isConnected = err == nil
		rc.httpResp = httpResp
		rc.mu.Unlock()

		if err == nil {
			if rc.hasSubscribeHandler() {
				if err := rc.SubscribeHandler(); err != nil {
					log.Fatalf("Dial: connect handler failed with %s", err.Error())
				}
				if !rc.getNonVerbose() {
					log.Printf("Dial: connect handler was successfully established with %s\n", rc.url)
				}
			} else {
				if !rc.getNonVerbose() {
					log.Printf("Dial: connection was successfully established with %s\n", rc.url)
				}
			}

			rc.setIsReconnecting(false)
			if rc.getKeepAliveTimeout() > 0 {
				rc.keepAlive()
			}

			return
		}

		if !rc.getNonVerbose() {
			log.Println(err)
			log.Println("Dial: will try again in", nextItvl, "seconds.")
		}

		time.Sleep(nextItvl)
	}
}

// GetHTTPResponse returns the http response from the handshake.
// Useful when WebSocket handshake fails,
// so that callers can handle redirects, authentication, etc.
func (rc *RecConn) GetHTTPResponse() *http.Response {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.httpResp
}

// GetDialError returns the last dialer error.
// nil on successful connection.
func (rc *RecConn) GetDialError() error {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.dialErr
}

// IsConnected returns the WebSocket connection state
func (rc *RecConn) IsConnected() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.isConnected
}

func (rc *RecConn) setIsReconnecting(isReconnecting bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.isReconnecting = isReconnecting
}

// IsReconnecting returns whether the connection is being reinstated
func (rc *RecConn) IsReconnecting() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.isReconnecting
}
