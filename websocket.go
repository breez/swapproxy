package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type WebSocketProxy struct {
	upstreamURL string
	clients     map[*websocket.Conn]map[string]bool // Tracks swap IDs for each client
	subscribers map[string]map[*websocket.Conn]bool // Tracks clients for each swap ID
	mu          sync.Mutex
	upstream    *websocket.Conn
}

func NewWebSocketProxy(upstreamURL string) *WebSocketProxy {
	return &WebSocketProxy{
		upstreamURL: upstreamURL,
		clients:     make(map[*websocket.Conn]map[string]bool),
		subscribers: make(map[string]map[*websocket.Conn]bool),
	}
}

func (p *WebSocketProxy) connectToUpstream() error {
	conn, _, err := websocket.Dial(context.Background(), p.upstreamURL, nil)
	if err != nil {
		return err
	}
	p.upstream = conn
	return nil
}

func (p *WebSocketProxy) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.CloseNow()

	// Set a read timeout (e.g., 30 seconds)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel() // Ensure the context is canceled on all paths

	p.mu.Lock()
	p.clients[conn] = make(map[string]bool) // Initialize swap IDs for this client
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()

		// Remove client from all subscriptions
		for swapID := range p.clients[conn] {
			delete(p.subscribers[swapID], conn)
			if len(p.subscribers[swapID]) == 0 {
				// If no more clients are subscribed to this swap ID, remove it from the map
				delete(p.subscribers, swapID)

				// Optionally, send an unsubscribe message to the upstream server
				if p.upstream != nil {
					unsubscribeMsg := map[string]any{
						"op":      "unsubscribe",
						"channel": "swap.update",
						"args":    []string{swapID},
					}
					messageBytes, err := json.Marshal(unsubscribeMsg)
					if err != nil {
						log.Printf("Failed to marshal unsubscribe message: %v", err)
						continue
					}
					err = p.upstream.Write(context.Background(), websocket.MessageText, messageBytes)
					if err != nil {
						log.Printf("Failed to forward unsubscribe message to upstream: %v", err)
					}
				}
			}
		}

		// Remove the client from the clients map
		delete(p.clients, conn)
	}()

	for {
		// Read message from client with timeout
		_, message, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				log.Printf("Client closed the connection: %v", err)
			} else {
				log.Printf("WebSocket read error: %v", err)
			}
			break
		}

		// Parse the message
		var msg map[string]any
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Failed to parse client message: %v", err)
			continue
		}

		// Handle subscription/unsubscription
		switch msg["op"] {
		case "subscribe":
			p.handleSubscribe(conn, msg)
		case "unsubscribe":
			p.handleUnsubscribe(conn, msg)
		default:
			log.Printf("Unknown operation: %s", msg["op"])
		}

		// Reset the timeout for the next read
		cancel() // Cancel the previous context
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel() // Ensure the new context is canceled on all paths
	}
}

func (p *WebSocketProxy) handleSubscribe(conn *websocket.Conn, msg map[string]any) {
	swapIDs, ok := msg["args"].([]any)
	if !ok {
		log.Printf("Invalid subscribe message: %v", msg)
		return
	}

	p.mu.Lock()
	for _, swapID := range swapIDs {
		id, ok := swapID.(string)
		if !ok {
			log.Printf("Invalid swap ID: %v", swapID)
			continue
		}

		// Add client to swap ID's subscribers
		if p.subscribers[id] == nil {
			p.subscribers[id] = make(map[*websocket.Conn]bool)
		}
		p.subscribers[id][conn] = true

		// Add swap ID to client's subscriptions
		p.clients[conn][id] = true
	}
	p.mu.Unlock() // Release the lock after modifying the maps

	// Forward subscribe message to upstream
	if p.upstream != nil {
		// Marshal the message into JSON ([]byte)
		messageBytes, err := json.Marshal(msg)
		if err != nil {
			log.Printf("Failed to marshal subscribe message: %v", err)
			return
		}

		// Write the message to the upstream WebSocket
		err = p.upstream.Write(context.Background(), websocket.MessageText, messageBytes)
		if err != nil {
			log.Printf("Failed to forward subscribe message to upstream: %v", err)
		}
	}
}

func (p *WebSocketProxy) handleUnsubscribe(conn *websocket.Conn, msg map[string]any) {
	swapIDs, ok := msg["args"].([]any)
	if !ok {
		log.Printf("Invalid unsubscribe message: %v", msg)
		return
	}

	p.mu.Lock()
	for _, swapID := range swapIDs {
		id, ok := swapID.(string)
		if !ok {
			log.Printf("Invalid swap ID: %v", swapID)
			continue
		}

		// Remove client from swap ID's subscribers
		delete(p.subscribers[id], conn)
		if len(p.subscribers[id]) == 0 {
			delete(p.subscribers, id)
		}

		// Remove swap ID from client's subscriptions
		delete(p.clients[conn], id)
	}
	p.mu.Unlock() // Release the lock after modifying the maps

	// Forward unsubscribe message to upstream
	if p.upstream != nil {
		// Marshal the message into JSON ([]byte)
		messageBytes, err := json.Marshal(msg)
		if err != nil {
			log.Printf("Failed to marshal unsubscribe message: %v", err)
			return
		}

		// Write the message to the upstream WebSocket
		err = p.upstream.Write(context.Background(), websocket.MessageText, messageBytes)
		if err != nil {
			log.Printf("Failed to forward unsubscribe message to upstream: %v", err)
		}
	}
}

func (p *WebSocketProxy) Start() {
	// Connect to upstream WebSocket server
	err := p.connectToUpstream()
	if err != nil {
		log.Fatalf("Failed to connect to upstream WebSocket: %v", err)
	}

	// Start a goroutine to listen for messages from the upstream server
	go func() {
		for {
			_, message, err := p.upstream.Read(context.Background())
			if err != nil {
				log.Printf("Upstream WebSocket read error: %v", err)

				// Attempt to reconnect to the upstream server
				for {
					log.Println("Attempting to reconnect to upstream server...")
					err := p.connectToUpstream()
					if err == nil {
						log.Println("Reconnected to upstream server successfully")

						// Resubscribe to all swap IDs after reconnection
						p.mu.Lock()
						swapIDs := make([]string, 0, len(p.subscribers))
						for swapID := range p.subscribers {
							swapIDs = append(swapIDs, swapID)
						}
						p.mu.Unlock()

						if len(swapIDs) > 0 {
							resubscribeMsg := map[string]any{
								"op":      "subscribe",
								"channel": "swap.update",
								"args":    swapIDs,
							}
							messageBytes, err := json.Marshal(resubscribeMsg)
							if err != nil {
								log.Printf("Failed to marshal resubscribe message: %v", err)
							} else {
								err = p.upstream.Write(context.Background(), websocket.MessageText, messageBytes)
								if err != nil {
									log.Printf("Failed to forward resubscribe message to upstream: %v", err)
								} else {
									log.Printf("Resubscribed to swap IDs: %v", swapIDs)
								}
							}
						}

						break
					}
					log.Printf("Failed to reconnect to upstream server: %v", err)

					// Wait before retrying
					time.Sleep(5 * time.Second)
				}

				// Continue the loop to resume processing messages
				continue
			}

			// Parse the message
			var msg map[string]any
			if err := json.Unmarshal(message, &msg); err != nil {
				log.Printf("Failed to parse upstream message: %v", err)
				continue
			}

			// Extract swap IDs from the message
			args, ok := msg["args"].([]any)
			if !ok {
				log.Printf("Invalid upstream message: %v", msg)
				continue
			}

			// Collect clients to notify
			clientsToNotify := make(map[*websocket.Conn][]byte)
			p.mu.Lock()
			for _, arg := range args {
				swap, ok := arg.(map[string]any)
				if !ok {
					continue
				}
				swapID, ok := swap["id"].(string)
				if !ok {
					continue
				}

				// Marshal the message into JSON ([]byte)
				messageBytes, err := json.Marshal(msg)
				if err != nil {
					log.Printf("Failed to marshal message for client: %v", err)
					continue
				}

				// Add clients to notify
				for client := range p.subscribers[swapID] {
					clientsToNotify[client] = messageBytes
				}
			}
			p.mu.Unlock() // Release the lock after collecting clients to notify

			// Notify clients
			for client, messageBytes := range clientsToNotify {
				err := client.Write(context.Background(), websocket.MessageText, messageBytes)
				if err != nil {
					log.Printf("Failed to send message to client: %v", err)
					client.CloseNow()
					p.mu.Lock()
					delete(p.clients, client)
					for swapID := range p.subscribers {
						delete(p.subscribers[swapID], client)
						if len(p.subscribers[swapID]) == 0 {
							delete(p.subscribers, swapID)
						}
					}
					p.mu.Unlock()
				}
			}
		}
	}()
}
