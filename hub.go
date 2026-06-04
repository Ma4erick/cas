package main

import (
	"context"
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
	userID    string
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
				// Only fire "joined" if the user wasn't already joined in the DB.
				// This prevents re-announcing on every page reload.
				alreadyJoined := false
				if DB != nil && client.userID != "" {
					statuses, _ := GetUserSessionStatuses(context.Background(), client.userID)
					alreadyJoined = statuses[client.sessionID] == "joined"
				}
				if !alreadyJoined {
					logVerbose("ws join: %s → session %s", client.name, client.sessionID)
					h.BroadcastToSession(client.sessionID, WSMessage{
						Type: "system",
						Text: client.name + " joined",
					})
				}
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
	if RDB != nil {
		logVerbose("redis publish: session=%s type=%s", sessionID, msg.Type)
		RDB.Publish(context.Background(), redisChanSession+sessionID, data)
		return
	}
	h.broadcast <- broadcastMsg{sessionID: sessionID, payload: data}
}

func (h *Hub) BroadcastAll(msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	if RDB != nil {
		logVerbose("redis publish: channel=%s type=%s", redisChanAll, msg.Type)
		RDB.Publish(context.Background(), redisChanAll, data)
		return
	}
	h.broadcast <- broadcastMsg{payload: data}
}

// StartRedisSubscriber listens on Redis channels and forwards messages to
// locally connected clients. Must be called after RDB is set.
func (h *Hub) StartRedisSubscriber(ctx context.Context) {
	if RDB == nil {
		return
	}
	sub := RDB.PSubscribe(ctx,
		redisChanAll,
		redisChanSession+"*",
	)
	go func() {
		defer sub.Close()
		ch := sub.Channel()
		for msg := range ch {
			payload := []byte(msg.Payload)
			if msg.Channel == redisChanAll {
				logVerbose("redis receive: channel=%s", redisChanAll)
				h.broadcast <- broadcastMsg{payload: payload}
			} else {
				sessionID := msg.Channel[len(redisChanSession):]
				logVerbose("redis receive: session=%s", sessionID)
				h.broadcast <- broadcastMsg{sessionID: sessionID, payload: payload}
			}
		}
	}()
	log.Printf("Redis pub/sub subscriber started")
}

// ActiveUserIDs returns the user IDs of all connected clients in a session.
func (h *Hub) ActiveUserIDs(sessionID string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := map[string]bool{}
	var ids []string
	for client := range h.sessions[sessionID] {
		if client.userID != "" && !seen[client.userID] {
			seen[client.userID] = true
			ids = append(ids, client.userID)
		}
	}
	return ids
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

	userID := ""
	if c, err := r.Cookie("cas-user-id"); err == nil {
		userID = c.Value
	}
	client := &Client{
		id:        uuid.New().String(),
		sessionID: sessionID,
		userID:    userID,
		name:      r.URL.Query().Get("name"),
		announce:  r.URL.Query().Get("announce") == "1",
		conn:      conn,
		send:      make(chan []byte, 64),
		hub:       h,
	}

	h.register <- client

	// Mark session as read for this user.
	if DB != nil {
		if c, err := r.Cookie("cas-user-id"); err == nil && c.Value != "" {
			MarkSessionRead(r.Context(), c.Value, sessionID)
		}
	}

	// Lazy-load messages from DB on first connection to this session.
	session.mu.Lock()
	if !session.messagesLoaded && DB != nil {
		if dbMsgs, err := DBGetMessages(r.Context(), session.ID); err == nil {
			session.Messages = make([]Message, 0, len(dbMsgs))
			for _, m := range dbMsgs {
				session.Messages = append(session.Messages, Message{
					ID: m.ID, Role: m.Role, Content: m.Content,
					Sender: m.Sender, SenderColor: m.SenderColor, Timestamp: m.CreatedAt,
				})
			}
			session.messagesLoaded = true
		}
	}
	session.mu.Unlock()

	// Send history to the connecting client.
	history := session.GetMessages()
	data, _ := json.Marshal(WSMessage{Type: "history", History: history})
	client.send <- data

	go client.writePump()
	go client.readPump()
}
