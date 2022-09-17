package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/recws-org/recws"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	ws := recws.RecConn{
		KeepAliveTimeout: 10 * time.Second,
	}
	ws.Dial("wss://echo.websocket.org", nil)

	go func() {
		time.Sleep(2 * time.Second)
		cancel()
	}()

	for {
		select {
		case <-ctx.Done():
			go ws.Close()
			log.Printf("Websocket closed %s", ws.GetURL())
			return
		default:
			if !ws.IsConnected() {
				log.Printf("Websocket disconnected %s", ws.GetURL())
				time.Sleep(time.Millisecond * 100) // some release cpu while waiting for reconnect
				continue
			}

			if err := ws.WriteMessage(1, []byte("Incoming")); err != nil {
				log.Printf("Error: WriteMessage %s", ws.GetURL())
				return
			}

			_, message, err := ws.ReadMessage()
			if err != nil {
				log.Printf("Error: ReadMessage %s", ws.GetURL())
				return
			}
			if err != nil {
				if !errors.Is(err, recws.ErrNotConnected) {
					log.Printf("Error: ReadMessage %s", ws.GetURL())
					time.Sleep(time.Second * 5) // throttle repeated errors
				}
				continue // go to next read
			}

			log.Printf("Success: %s", message)
		}
	}
}
