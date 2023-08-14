package main

import (
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

type WebsocketPacket struct {
	ClientID    uint64
	MessageType uint64
	Message     string
}

type WebsocketCallback func(WebsocketPacket)

type WebsocketHandler struct {
	conn      *websocket.Conn
	writeLock sync.RWMutex
}

func NewWSHandler(addr string, path string) *WebsocketHandler {
	u := url.URL{Scheme: "ws", Host: addr, Path: path}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatal("Dial:", err)
	}
	return &WebsocketHandler{conn, sync.RWMutex{}}
}

func (w *WebsocketHandler) StartListening(cb WebsocketCallback) {
	go func() {
		for {
			_, message, err := w.conn.ReadMessage()
			if err != nil {
				panic(err)
			}
			v := strings.Split(string(message), "@")
			clientID, _ := strconv.ParseUint(v[0], 10, 64)
			messageType, _ := strconv.ParseUint(v[1], 10, 64)
			wsPacket := WebsocketPacket{clientID, messageType, v[2]}
			println("Message: ", clientID, messageType)
			cb(wsPacket)
		}
	}()
}

func (w *WebsocketHandler) SendMessage(wsPacket WebsocketPacket) {
	s := fmt.Sprintf("%d@%d@%s", wsPacket.ClientID, wsPacket.MessageType, wsPacket.Message)
	w.writeLock.Lock()
	err := w.conn.WriteMessage(websocket.TextMessage, []byte(s))
	w.writeLock.Unlock()
	if err != nil {
		panic(err)
	}
}
