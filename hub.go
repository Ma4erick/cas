package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type WSMessage struct {
	Type       string      `json:"type"`
	Message    *Message    `json:"message,omitempty"`
	MessageID  string      `json:"messageId,omitempty"`
	Delta      string      `json:"delta,omitempty"`
	Error      string      `json:"error,omitempty"`
	Sessions   interface{} `json:"sessions,omitempty"`
	History    []Message   `json:"history,omitempty"`
	ToolName   string      `json:"toolName,omitempty"`
	ToolInput  string      `json:"toolInput,omitempty"`
	ToolOutput string      `json:"toolOutput,omitempty"`
	IsError    bool        `json:"isError,omitempty"`
	Text       string      `json:"text,omitempty"`
}

type Client struct {
	id        string
	sessionID string
	name      string
	announce  bool // true only on first join / explicit leave
	conn      *websocket.Conn
	send      chan []byte
	hub       *Hub
}

func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

type Hub struct {
	// sessionID -> set of clients
	sessions   map[string]map[*Client]bool
	register   chan *Client
	unregister chan *Client
	broadcast  chan broadcastMsg
	mu         sync.RWMutex
}

type broadcastMsg struct {
	sessionID string // empty = all sessions
	payload   []byte
}

func NewHub() *Hub {
	return &Hub{
		sessions:   make(map[string]map[*Client]bool),
		register:   make(chan *Client, 16),
		unregister: make(chan *Client, 16),
		broadcast:  make(chan broadcastMsg, 256),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			if h.sessions[client.sessionID] == nil {
				h.sessions[client.sessionID] = make(map[*Client]bool)
			}
			h.sessions[client.sessionID][client] = true
			h.mu.Unlock()
			if client.name != "" && client.announce {
				h.BroadcastToSession(client.sessionID, WSMessage{
					Type: "system",
					Text: client.name + " joined",
				})
			}

		case client := <-h.unregister:
			h.mu.Lock()
			if clients, ok := h.sessions[client.sessionID]; ok {
				if _, ok := clients[client]; ok {
					delete(clients, client)
					close(client.send)
					if len(clients) == 0 {
						delete(h.sessions, client.sessionID)
					}
				}
			}
			h.mu.Unlock()
			if client.name != "" && client.announce {
				h.BroadcastToSession(client.sessionID, WSMessage{
					Type: "system",
					Text: client.name + " left",
				})
			}

		case msg := <-h.broadcast:
			h.mu.RLock()
			if msg.sessionID == "" {
				// Broadcast to all clients across all sessions
				for _, clients := range h.sessions {
					for client := range clients {
						select {
						case client.send <- msg.payload:
						default:
							log.Printf("client send buffer full, dropping message")
						}
					}
				}
			} else {
				for client := range h.sessions[msg.sessionID] {
					select {
					case client.send <- msg.payload:
					default:
						log.Printf("client send buffer full, dropping message")
					}
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *Hub) BroadcastToSession(sessionID string, msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.broadcast <- broadcastMsg{sessionID: sessionID, payload: data}
}

func (h *Hub) BroadcastAll(msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.broadcast <- broadcastMsg{payload: data}
}

func (h *Hub) BroadcastSessionList(sessions interface{}) {
	h.BroadcastAll(WSMessage{Type: "session_list", Sessions: sessions})
}

func (h *Hub) ServeWS(sm *SessionManager, sessionID string, w http.ResponseWriter, r *http.Request) {
	session, ok := sm.GetSession(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}

	client := &Client{
		id:        uuid.New().String(),
		sessionID: sessionID,
		name:      r.URL.Query().Get("name"),
		announce:  r.URL.Query().Get("announce") == "1",
		conn:      conn,
		send:      make(chan []byte, 64),
		hub:       h,
	}

	h.register <- client

	// Send history immediately
	history := session.GetMessages()
	data, _ := json.Marshal(WSMessage{Type: "history", History: history})
	client.send <- data

	go client.writePump()
	go client.readPump()
}
