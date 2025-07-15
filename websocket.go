package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/exp/maps"
)

type Subscription struct {
	channel string
	arg     any
}

type WebSocketProxy struct {
	upstreamURL   string
	clients       map[*websocket.Conn]map[string]bool // Tracks ids for each client
	subscribers   map[string]map[*websocket.Conn]bool // Tracks clients for each id
	subscriptions map[string]*Subscription            // Store subscription data
	mu            sync.Mutex
	upstream      *websocket.Conn
	config        *Config
}

func NewWebSocketProxy(upstreamURL string, config *Config) *WebSocketProxy {
	// Parse the URL to add additional parameters
	wsURL, err := url.Parse(upstreamURL)
	if err != nil {
		log.Fatalf("Invalid WebSocket URL: %v", err)
	}

	// Add additional parameters to the WebSocket URL
	query := wsURL.Query()
	for _, param := range config.WebSocketAdditionalParams {
		if len(param) == 2 {
			query.Add(param[0], param[1])
		}
	}
	wsURL.RawQuery = query.Encode()

	return &WebSocketProxy{
		upstreamURL:   wsURL.String(),
		clients:       make(map[*websocket.Conn]map[string]bool),
		subscribers:   make(map[string]map[*websocket.Conn]bool),
		subscriptions: make(map[string]*Subscription),
		config:        config,
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
	// Create options to accept connections from any origin
    options := &websocket.AcceptOptions{
        OriginPatterns: []string{"*"}, // Allow all origins
    }
	conn, err := websocket.Accept(w, r, options)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.CloseNow()

	if p.config.CACert != nil {
		apiKeyCtx, apiKeyCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer apiKeyCancel()

		// Read the first message to get the API key
		_, message, err := conn.Read(apiKeyCtx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				log.Printf("Client did not send API key within 2 seconds")
			} else {
				log.Printf("Failed to read initial message: %v", err)
			}
			conn.CloseNow()
			return
		}

		// Parse the message
		var apiKeyMsg struct {
			ApiKey string `json:"apikey"`
		}
		if err := json.Unmarshal(message, &apiKeyMsg); err != nil {
			log.Printf("Failed to parse initial message: %v", err)
			// Send error message before closing
			errMsg := map[string]any{
				"event": "error",
				"error": "no API key provided",
			}
			messageBytes, _ := json.Marshal(errMsg)
			conn.Write(context.Background(), websocket.MessageText, messageBytes)
			conn.CloseNow()
			return
		}

		log.Printf("Received API key (length): %d", len(apiKeyMsg.ApiKey))

		if !validateAPIKey(p.config.CACert, apiKeyMsg.ApiKey) {
			log.Printf("WebSocket connection rejected: invalid API key")
			// Send error message and close connection
			errMsg := map[string]any{
				"event": "error",
				"error": "invalid API key",
			}
			messageBytes, _ := json.Marshal(errMsg)
			conn.Write(context.Background(), websocket.MessageText, messageBytes)
			conn.CloseNow()
			return
		}
	}

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
		for id := range p.clients[conn] {
			delete(p.subscribers[id], conn)
			if len(p.subscribers[id]) == 0 {
				// If no more clients are subscribed to this id, remove it from the map
				delete(p.subscribers, id)

				// Optionally, send an unsubscribe message to the upstream server
				if p.upstream != nil && p.subscriptions[id] != nil {
					subscription := p.subscriptions[id]
					unsubscribeMsg := map[string]any{
						"op":      "unsubscribe",
						"channel": subscription.channel,
						"args":    []string{id},
					}
					// Release the lock before writing to the upstream WebSocket
					p.mu.Unlock()
					if err = p.sendUpstreamRequest(unsubscribeMsg); err != nil {
						log.Printf("Failed to send unsubscribe: %v", err)
					}
					p.mu.Lock() // Reacquire the lock after writing
				}

				delete(p.subscriptions, id)
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

		// Handle operation
		switch msg["op"] {
		case "subscribe":
			p.handleSubscribe(conn, msg)
		case "unsubscribe":
			p.handleUnsubscribe(conn, msg)
		case "invoice", "invoice.error":
			p.sendUpstreamRequest(msg)
		case "ping":
			// Respond with a pong message
			pongMsg := map[string]any{
				"event": "pong",
			}
			if err := p.sendClientResponse(conn, pongMsg); err != nil {
				log.Printf("Failed to send pong: %v", err)
			}
		default:
			log.Printf("Unknown operation: %s", msg["op"])
		}

		// Reset the timeout for the next read
		cancel() // Cancel the previous context
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel() // Ensure the new context is canceled on all paths
	}
}

func currentTimestampMillis() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

func (p *WebSocketProxy) handleSubscribe(conn *websocket.Conn, msg map[string]any) {
	channel, channelOk := msg["channel"].(string)
	args, ok := msg["args"].([]any)
	if !ok || !channelOk {
		log.Printf("Invalid subscribe message: %v", msg)
		return
	}

	var subscribeArgs []any
	p.mu.Lock()
	for _, arg := range args {
		var id string
		switch channel {
		case "invoice.request":
			// Extract offer from invoice.request
			params, ok := arg.(map[string]any)
			if !ok {
				log.Printf("Invalid invoice request params: %v", arg)
				continue
			}
			id, ok = params["offer"].(string)
			if !ok {
				log.Printf("Invalid offer: %v", arg)
				continue
			}
		case "swap.update":
			// Extract swap ID from swap.update
			id, ok = arg.(string)
			if !ok {
				log.Printf("Invalid swap ID: %v", arg)
				continue
			}
		default:
			log.Printf("Unknown channel: %s", channel)
			continue
		}

		// Add client to offer's subscribers
		if p.subscribers[id] == nil {
			p.subscribers[id] = make(map[*websocket.Conn]bool)
			// Add the arg to the list of args to subscribe to
			subscribeArgs = append(subscribeArgs, arg)
		}
		p.subscribers[id][conn] = true

		// Add subscription data
		if p.subscriptions[id] == nil {
			p.subscriptions[id] = &Subscription{
				channel: channel,
				arg:     arg,
			}
		}

		// Add offer to client's subscriptions
		p.clients[conn][id] = true
	}
	p.mu.Unlock() // Release the lock after modifying the maps

	// Only send subscribe message with necessary args to upstream
	if len(subscribeArgs) > 0 {
		subscribeMsg := map[string]any{
			"op":      "subscribe",
			"channel": channel,
			"args":    subscribeArgs,
		}
		if err := p.sendUpstreamRequest(subscribeMsg); err != nil {
			log.Printf("Failed to send subscribe: %v", err)
		}
	}

	// Send subscribe message to client
	subscribeMsg := map[string]any{
		"event":     "subscribe",
		"channel":   channel,
		"args":      args,
		"timestamp": currentTimestampMillis(),
	}

	// For invoice.request channel, extract just the offer strings to match the upstream format
	if channel == "invoice.request" {
		var offers []string
		for _, arg := range args {
			if params, ok := arg.(map[string]any); ok {
				if offer, ok := params["offer"].(string); ok {
					offers = append(offers, offer)
				}
			}
		}
		subscribeMsg["args"] = offers
	}

	if err := p.sendClientResponse(conn, subscribeMsg); err != nil {
		log.Printf("Failed to send subscribe: %v", err)
	}
}

func (p *WebSocketProxy) handleUnsubscribe(conn *websocket.Conn, msg map[string]any) {
	// The args can be either a swap ID or an offer
	channel, channelOk := msg["channel"].(string)
	args, ok := msg["args"].([]any)
	if !ok || !channelOk {
		log.Printf("Invalid unsubscribe message: %v", msg)
		return
	}

	var unsubscribeIds []string
	p.mu.Lock()
	for _, arg := range args {
		id, ok := arg.(string)
		if !ok {
			log.Printf("Invalid id: %v", arg)
			continue
		}

		// Remove client from id's subscribers
		delete(p.subscribers[id], conn)
		if len(p.subscribers[id]) == 0 {
			delete(p.subscribers, id)
			// If there are no more subscribers for this id,
			// it's safe to remove it from subscriptions
			delete(p.subscriptions, id)
			// Add the id to the list of ids to unsubscribe from
			unsubscribeIds = append(unsubscribeIds, id)
		}

		// Remove id from client's subscriptions
		delete(p.clients[conn], id)
	}
	p.mu.Unlock() // Release the lock after modifying the maps

	// Only send unsubscribe message with necessary args to upstream
	if len(unsubscribeIds) > 0 {
		unsubscribeMsg := map[string]any{
			"op":      "unsubscribe",
			"channel": channel,
			"args":    unsubscribeIds,
		}
		if err := p.sendUpstreamRequest(unsubscribeMsg); err != nil {
			log.Printf("Failed to send unsubscribe: %v", err)
		}
	}

	// Send unsubscribe message to client
	unsubscribeMsg := map[string]any{
		"event":     "unsubscribe",
		"channel":   channel,
		"args":      args,
		"timestamp": currentTimestampMillis(),
	}
	if err := p.sendClientResponse(conn, unsubscribeMsg); err != nil {
		log.Printf("Failed to send unsubscribe: %v", err)
	}
}

func (p *WebSocketProxy) sendUpstreamRequest(msg map[string]any) error {
	if p.upstream != nil {
		// Marshal the message into JSON ([]byte)
		messageBytes, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("Failed to marshal message: %v", err)
		}

		// Write the message to the upstream WebSocket
		err = p.upstream.Write(context.Background(), websocket.MessageText, messageBytes)
		if err != nil {
			return fmt.Errorf("Failed to forward message to upstream: %v", err)
		}
	}
	return nil
}

func (p *WebSocketProxy) sendClientResponse(conn *websocket.Conn, msg map[string]any) error {
	// Marshal the message into JSON ([]byte)
	messageBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("Failed to marshal message: %v", err)
	}

	// Write the message to the client WebSocket
	err = conn.Write(context.Background(), websocket.MessageText, messageBytes)
	if err != nil {
		return fmt.Errorf("Failed to send message to client: %v", err)
	}
	return nil
}

func filterSubcriptionArgs(subscriptions map[string]*Subscription, channel string) []any {
	var filtered []any
	for _, subscription := range subscriptions {
		if subscription.channel == channel {
			filtered = append(filtered, subscription.arg)
		}
	}
	return filtered
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

						p.mu.Lock()
						swapIdArgs := filterSubcriptionArgs(p.subscriptions, "swap.update")
						offerArgs := filterSubcriptionArgs(p.subscriptions, "invoice.request")
						p.mu.Unlock()

						// Resubscribe to all swaps after reconnection
						if len(swapIdArgs) > 0 {
							resubscribeMsg := map[string]any{
								"op":      "subscribe",
								"channel": "swap.update",
								"args":    swapIdArgs,
							}
							if err = p.sendUpstreamRequest(resubscribeMsg); err != nil {
								log.Printf("Failed to resubscribe to swap updates: %v", err)
							}
						}
						// Resubscribe to all offers after reconnection
						if len(offerArgs) > 0 {
							resubscribeMsg := map[string]any{
								"op":      "subscribe",
								"channel": "invoice.request",
								"args":    offerArgs,
							}
							if err = p.sendUpstreamRequest(resubscribeMsg); err != nil {
								log.Printf("Failed to resubscribe to invoice requests: %v", err)
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

			event, ok := msg["event"].(string)
			if !ok {
				log.Printf("Invalid upstream message: %v", msg)
				continue
			}

			// Group updates by client
			clientUpdates := make(map[*websocket.Conn][]map[string]any)
			p.mu.Lock()
			switch event {
			case "subscribe", "unsubscribe":
				log.Printf("Received subscribe/unsubscribe response: %s", msg)
			case "error":
				log.Printf("Received error response: %s", msg)
			case "update":
				args, ok := msg["args"].([]any)
				if !ok {
					log.Printf("Invalid upstream message: %v", msg)
					continue
				}
				for _, arg := range args {
					swapStatus, ok := arg.(map[string]any)
					if !ok {
						continue
					}
					id, ok := swapStatus["id"].(string)
					if !ok {
						continue
					}
					// Add updates to each subscribed client
					for client := range p.subscribers[id] {
						clientUpdates[client] = append(clientUpdates[client], swapStatus)
					}
				}
			case "request":
				args, ok := msg["args"].([]any)
				if !ok {
					log.Printf("Invalid upstream message: %v", msg)
					continue
				}
				for _, arg := range args {
					invoiceRequest, ok := arg.(map[string]any)
					if !ok {
						continue
					}
					id, ok := invoiceRequest["offer"].(string)
					if !ok {
						continue
					}
					// Boltz only sends the invoice request to the last subscribed client
					if len(p.subscribers[id]) > 0 {
						client := maps.Keys(p.subscribers[id])[0]
						clientUpdates[client] = append(clientUpdates[client], invoiceRequest)
					}
				}
			default:
				log.Printf("Unknown response: %s", msg)
				continue
			}
			p.mu.Unlock() // Release the lock after grouping updates

			// Notify clients
			for client, updates := range clientUpdates {
				// Create a single notification message for the client
				notification := map[string]any{
					"event":     msg["event"],
					"channel":   msg["channel"],
					"args":      updates,
					"timestamp": currentTimestampMillis(),
				}

				// Marshal the message into JSON ([]byte)
				messageBytes, err := json.Marshal(notification)
				if err != nil {
					log.Printf("Failed to marshal notification for client: %v", err)
					continue
				}

				// Write the message to the client WebSocket
				err = client.Write(context.Background(), websocket.MessageText, messageBytes)
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
