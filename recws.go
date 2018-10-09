// Package recws provides websocket client based on gorilla/websocket
// that will automatically reconnect if the connection is dropped.
package recws

import (
	"errors"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
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
	// NonVerbose suppress connecting/reconnecting messages.
	NonVerbose bool
	// SubscribeHandler fires after the connection successfully establish.
	SubscribeHandler func(rc *RecConn) error

	mu          sync.Mutex
	url         string
	reqHeader   http.Header
	httpResp    *http.Response
	dialErr     error
	isConnected bool
	dialer      *websocket.Dialer

	*websocket.Conn
}

// CloseAndReconnect will try to reconnect.
func (rc *RecConn) closeAndReconnect() {
	rc.Close()
	go rc.connect()
}

// Close closes the underlying network connection without
// sending or waiting for a close frame.
func (rc *RecConn) Close() {
	rc.mu.Lock()
	if rc.Conn != nil {
		rc.Conn.Close()
	}
	rc.isConnected = false
	rc.mu.Unlock()
}

// ReadMessage is a helper method for getting a reader
// using NextReader and reading from that reader to a buffer.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) ReadMessage() (messageType int, message []byte, err error) {
	err = ErrNotConnected
	if rc.IsConnected() {
		messageType, message, err = rc.Conn.ReadMessage()
		if err != nil {
			rc.closeAndReconnect()
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
		err = rc.Conn.WriteMessage(messageType, data)
		if err != nil {
			rc.closeAndReconnect()
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
		err = rc.Conn.WriteJSON(v)
		if err != nil {
			rc.closeAndReconnect()
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
		if err != nil {
			rc.closeAndReconnect()
		}
	}

	return err
}

// Dial creates a new client connection.
// The URL url specifies the host and request URI. Use requestHeader to specify
// the origin (Origin), subprotocols (Sec-WebSocket-Protocol) and cookies
// (Cookie). Use GetHTTPResponse() method for the response.Header to get
// the selected subprotocol (Sec-WebSocket-Protocol) and cookies (Set-Cookie).
func (rc *RecConn) Dial(urlStr string, reqHeader http.Header) {
	if urlStr == "" {
		log.Fatal("Dial: Url cannot be empty")
	}
	u, err := url.Parse(urlStr)

	if err != nil {
		log.Fatal("Url:", err)
	}

	if u.Scheme != "ws" && u.Scheme != "wss" {
		log.Fatal("Url: websocket URIs must start with ws or wss scheme")
	}

	if u.User != nil {
		log.Fatal("Url: user name and password are not allowed in websocket URIs")
	}

	rc.url = urlStr

	if rc.RecIntvlMin == 0 {
		rc.RecIntvlMin = 2 * time.Second
	}

	if rc.RecIntvlMax == 0 {
		rc.RecIntvlMax = 30 * time.Second
	}

	if rc.RecIntvlFactor == 0 {
		rc.RecIntvlFactor = 1.5
	}

	if rc.HandshakeTimeout == 0 {
		rc.HandshakeTimeout = 2 * time.Second
	}

	rc.dialer = websocket.DefaultDialer
	rc.dialer.HandshakeTimeout = rc.HandshakeTimeout

	go rc.connect()

	// wait on first attempt
	time.Sleep(rc.HandshakeTimeout)
}

// GetURL returns current connection url
func (rc *RecConn) GetURL() string {
	return rc.url
}

func (rc *RecConn) connect() {
	b := &backoff.Backoff{
		Min:    rc.RecIntvlMin,
		Max:    rc.RecIntvlMax,
		Factor: rc.RecIntvlFactor,
		Jitter: true,
	}

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
			if !rc.NonVerbose {
				log.Printf("Dial: connection was successfully established with %s\n", rc.url)

				if rc.SubscribeHandler == nil {
					return
				}

				if err := rc.SubscribeHandler(rc); err != nil {
					log.Fatalf("Dial: connect handler failed with %s", err.Error())
				}

				log.Printf("Dial: connect handler was successfully established with %s\n", rc.url)
			}

			return
		}

		if !rc.NonVerbose {
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
	rc.mu.Lock()
	defer rc.mu.Unlock()

	return rc.httpResp
}

// GetDialError returns the last dialer error.
// nil on successful connection.
func (rc *RecConn) GetDialError() error {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	return rc.dialErr
}

// IsConnected returns the WebSocket connection state
func (rc *RecConn) IsConnected() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	return rc.isConnected
}
