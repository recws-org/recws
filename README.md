# recws

[![Go Report Card](https://goreportcard.com/badge/github.com/mariuspass/recws)](https://goreportcard.com/report/github.com/mariuspass/recws)
[![GitHub license](https://img.shields.io/github/license/Naereen/StrapDown.js.svg)](https://github.com/Naereen/StrapDown.js/blob/master/LICENSE)

Reconnecting WebSocket is a websocket client based on [gorilla/websocket](https://github.com/gorilla/websocket) that will automatically reconnect if the connection is dropped.

## Basic example

```go
package main

import (
	"context"
	"github.com/mariuspass/recws"
	"log"
	"time"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	ws := recws.RecConn{}
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

			log.Printf("Success: %s", message)
		}
	}
}
```
