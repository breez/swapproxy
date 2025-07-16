package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestEnvironment creates a mock upstream server and proxy for testing
func setupTestEnvironment(t *testing.T) (*httptest.Server, *WebSocketProxy, func()) {
	// Create a mock upstream server with proper connection handling
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		// Create a context that we can cancel
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Handle incoming messages
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_, message, err := conn.Read(ctx)
				if err != nil {
					return
				}

				// Parse the message
				var msg map[string]any
				if err := json.Unmarshal(message, &msg); err != nil {
					continue
				}

				// Handle different operations
				switch msg["op"] {
				case "subscribe":
					// Simulate subscription confirmation
					response := map[string]any{
						"event":   "subscribe",
						"channel": msg["channel"],
					}
					responseBytes, _ := json.Marshal(response)
					if err := conn.Write(ctx, websocket.MessageText, responseBytes); err != nil {
						return
					}

					// If it's a swap subscription, send some updates
					if msg["channel"] == "swap.update" {
						args, ok := msg["args"].([]any)
						if !ok {
							continue
						}
						for _, arg := range args {
							swapID, ok := arg.(string)
							if !ok {
								continue
							}
							// Send a few updates for this swap in a separate goroutine
							go func(id string) {
								for i := 0; i < 3; i++ {
									select {
									case <-ctx.Done():
										return
									default:
										update := map[string]any{
											"event":   "update",
											"channel": "swap.update",
											"args": []any{
												map[string]any{
													"id":     id,
													"status": []string{"pending", "mempool", "completed"}[i],
												},
											},
										}
										updateBytes, _ := json.Marshal(update)
										if err := conn.Write(ctx, websocket.MessageText, updateBytes); err != nil {
											return
										}
										time.Sleep(100 * time.Millisecond)
									}
								}
							}(swapID)
						}
					}
				case "unsubscribe":
					// Simulate unsubscribe confirmation
					response := map[string]any{
						"event":   "unsubscribe",
						"channel": msg["channel"],
					}
					responseBytes, _ := json.Marshal(response)
					if err := conn.Write(ctx, websocket.MessageText, responseBytes); err != nil {
						return
					}
				}
			}
		}
	}))

	// Create the proxy with a custom config
	config := &Config{
		WebSocketAdditionalParams: [][2]string{
			{"test", "true"},
		},
	}
	proxy := NewWebSocketProxy("ws"+strings.TrimPrefix(upstreamServer.URL, "http"), config)

	// Start the proxy with a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		proxy.Start()
		<-ctx.Done()
	}()

	// Wait for initial connection
	time.Sleep(500 * time.Millisecond)

	// Return cleanup function
	cleanup := func() {
		cancel()
		upstreamServer.Close()
	}

	return upstreamServer, proxy, cleanup
}

func TestIntegration_MultipleClients(t *testing.T) {
	_, proxy, cleanup := setupTestEnvironment(t)
	defer cleanup()

	// Create test server for proxy
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Connect multiple clients
	numClients := 3
	clients := make([]*websocket.Conn, numClients)
	for i := 0; i < numClients; i++ {
		client, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
		require.NoError(t, err)
		clients[i] = client
		defer client.CloseNow()
	}

	// Subscribe all clients to the same swap
	swapID := "test-swap"
	var wg sync.WaitGroup
	for _, client := range clients {
		wg.Add(1)
		go func(c *websocket.Conn) {
			defer wg.Done()

			// Subscribe
			subscribeMsg := map[string]any{
				"op":      "subscribe",
				"channel": "swap.update",
				"args":    []any{swapID},
			}
			subscribeBytes, _ := json.Marshal(subscribeMsg)
			err := c.Write(context.Background(), websocket.MessageText, subscribeBytes)
			require.NoError(t, err)

			// Read subscribe response
			_, message, err := c.Read(context.Background())
			require.NoError(t, err)
			var response map[string]any
			err = json.Unmarshal(message, &response)
			require.NoError(t, err)
			assert.Equal(t, "subscribe", response["event"])

			// Read updates
			for i := 0; i < 3; i++ {
				_, message, err := c.Read(context.Background())
				require.NoError(t, err)

				err = json.Unmarshal(message, &response)
				require.NoError(t, err)
				assert.Equal(t, "update", response["event"])
				assert.Equal(t, "swap.update", response["channel"])

				args, ok := response["args"].([]any)
				require.True(t, ok)
				require.Len(t, args, 1)

				swapStatus, ok := args[0].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, swapID, swapStatus["id"])
				status, ok := swapStatus["status"].(string)
				require.True(t, ok)
				assert.Equal(t, []string{"pending", "mempool", "completed"}[i], status)
			}
		}(client)
	}

	// Wait for all clients to finish with a timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Test completed successfully
	case <-time.After(15 * time.Second):
		t.Fatal("Test timed out waiting for all clients to finish")
	}
}

func TestIntegration_Reconnection(t *testing.T) {
	upstreamServer, proxy, cleanup := setupTestEnvironment(t)
	defer cleanup()

	// Create test server for proxy
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Connect client
	client, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()

	// Subscribe to a swap
	swapID := "test-swap"
	subscribeMsg := map[string]any{
		"op":      "subscribe",
		"channel": "swap.update",
		"args":    []any{swapID},
	}
	subscribeBytes, _ := json.Marshal(subscribeMsg)
	err = client.Write(context.Background(), websocket.MessageText, subscribeBytes)
	require.NoError(t, err)

	// Read subscribe response
	_, message, err := client.Read(context.Background())
	require.NoError(t, err)
	var response map[string]any
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "subscribe", response["event"])

	// Close upstream server to simulate connection loss
	upstreamServer.Close()

	// Create new upstream server BEFORE triggering reconnection
	newUpstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		// Create a context that we can cancel
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Handle incoming messages
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_, message, err := conn.Read(ctx)
				if err != nil {
					return
				}

				// Parse the message
				var msg map[string]any
				if err := json.Unmarshal(message, &msg); err != nil {
					continue
				}

				// Handle subscribe operation
				if msg["op"] == "subscribe" {
					// Send subscription confirmation
					response := map[string]any{
						"event":   "subscribe",
						"channel": msg["channel"],
					}
					responseBytes, _ := json.Marshal(response)
					if err := conn.Write(ctx, websocket.MessageText, responseBytes); err != nil {
						return
					}

					// Send an update with "completed" status
					update := map[string]any{
						"event":   "update",
						"channel": "swap.update",
						"args": []any{
							map[string]any{
								"id":     swapID,
								"status": "status_from_new_upstream",
							},
						},
					}
					updateBytes, _ := json.Marshal(update)
					if err := conn.Write(ctx, websocket.MessageText, updateBytes); err != nil {
						return
					}
				}
			}
		}
	}))
	defer newUpstreamServer.Close()

	// Update proxy's upstream URL and force reconnection by closing the current connection
	proxy.upstreamURL = "ws" + strings.TrimPrefix(newUpstreamServer.URL, "http")

	// Force close the current upstream connection to trigger reconnection
	if proxy.upstream != nil {
		proxy.upstream.CloseNow()
	}

	// Wait for reconnection to complete and resubscription to happen
	time.Sleep(2 * time.Second)

	// Read messages until we get the update with "status_from_new_upstream" status
	// We might receive multiple messages during reconnection
	timeout := time.After(10 * time.Second)

	for {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for status_from_new_upstream status update")
		default:
			// Set a short read timeout
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			_, message, err := client.Read(ctx)
			cancel()
			if err != nil {
				// Continue if it's a timeout, fail on other errors
				if websocket.CloseStatus(err) == websocket.StatusAbnormalClosure ||
					strings.Contains(err.Error(), "timeout") {
					continue
				}
				t.Fatalf("Error reading message: %v", err)
			}

			err = json.Unmarshal(message, &response)
			require.NoError(t, err)

			// Check if this is an update message
			if response["event"] == "update" && response["channel"] == "swap.update" {
				args, ok := response["args"].([]any)
				if ok && len(args) > 0 {
					swapStatus, ok := args[0].(map[string]any)
					if ok {
						id, idOk := swapStatus["id"].(string)
						status, statusOk := swapStatus["status"].(string)
						if idOk && statusOk && id == swapID && status == "status_from_new_upstream" {
							// Found the status_from_new_upstream status, test passed
							return
						}
					}
				}
			}
		}
	}
}
