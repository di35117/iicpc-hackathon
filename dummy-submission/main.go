package main

import (
	"fmt"
	"net/http"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	http.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			// Read the incoming order from the bot
			messageType, p, err := conn.ReadMessage()
			if err != nil {
				return
			}
			// Immediately bounce it back as an "ack"
			if err := conn.WriteMessage(messageType, p); err != nil {
				return
			}
		}
	})
	fmt.Println("WebSocket dummy engine running on :8080")
	http.ListenAndServe(":8080", nil)
}
