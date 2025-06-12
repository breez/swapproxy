package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generateTestCert creates a self-signed certificate for testing
func generateTestCert(t *testing.T) *x509.Certificate {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test Org"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour * 24),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(certDER)
	require.NoError(t, err)

	return cert
}

func TestNewWebSocketProxy(t *testing.T) {
	config := &Config{
		WebSocketAdditionalParams: [][2]string{
			{"key1", "value1"},
			{"key2", "value2"},
		},
	}

	proxy := NewWebSocketProxy("ws://example.com", config)
	assert.NotNil(t, proxy)
	assert.Equal(t, "ws://example.com?key1=value1&key2=value2", proxy.upstreamURL)
	assert.NotNil(t, proxy.clients)
	assert.NotNil(t, proxy.subscribers)
	assert.NotNil(t, proxy.subscriptions)
	assert.Equal(t, config, proxy.config)
}

func TestWebSocketProxy_HandleWebSocket_InvalidAuth(t *testing.T) {
	config := &Config{
		CACert: generateTestCert(t),
	}
	proxy := NewWebSocketProxy("ws://example.com", config)

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Try to connect without auth
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Read error message
	_, message, err := conn.Read(context.Background())
	require.NoError(t, err)

	var response1 map[string]any
	err = json.Unmarshal(message, &response1)
	require.NoError(t, err)
	assert.Equal(t, "error", response1["event"])
	assert.Equal(t, "invalid API key", response1["error"])

	// Try to connect with invalid auth
	conn2, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer invalid-key"},
		},
	})
	require.NoError(t, err)
	defer conn2.CloseNow()

	// Read error message
	_, message2, err := conn2.Read(context.Background())
	require.NoError(t, err)

	var response2 map[string]any
	err = json.Unmarshal(message2, &response2)
	require.NoError(t, err)
	assert.Equal(t, "error", response2["event"])
	assert.Equal(t, "invalid API key", response2["error"])
}

func TestWebSocketProxy_SubscribeUnsubscribe(t *testing.T) {
	config := &Config{}
	proxy := NewWebSocketProxy("ws://example.com", config)

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Connect client
	client, _, err := websocket.Dial(context.Background(), wsURL, nil)
	require.NoError(t, err)
	defer client.CloseNow()

	// Subscribe to a channel
	subscribeMsg := map[string]any{
		"op":      "subscribe",
		"channel": "swap.update",
		"args":    []any{"swap123"},
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
	assert.Equal(t, "swap.update", response["channel"])

	// Unsubscribe
	unsubscribeMsg := map[string]any{
		"op":      "unsubscribe",
		"channel": "swap.update",
		"args":    []any{"swap123"},
	}
	unsubscribeBytes, _ := json.Marshal(unsubscribeMsg)
	err = client.Write(context.Background(), websocket.MessageText, unsubscribeBytes)
	require.NoError(t, err)

	// Read unsubscribe response
	_, message, err = client.Read(context.Background())
	require.NoError(t, err)

	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "unsubscribe", response["event"])
	assert.Equal(t, "swap.update", response["channel"])
}

func TestWebSocketProxy_PingPong(t *testing.T) {
	config := &Config{}
	proxy := NewWebSocketProxy("ws://example.com", config)

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Connect client
	client, _, err := websocket.Dial(context.Background(), wsURL, nil)
	require.NoError(t, err)
	defer client.CloseNow()

	// Send ping
	pingMsg := map[string]any{
		"op": "ping",
	}
	pingBytes, _ := json.Marshal(pingMsg)
	err = client.Write(context.Background(), websocket.MessageText, pingBytes)
	require.NoError(t, err)

	// Read pong response
	_, message, err := client.Read(context.Background())
	require.NoError(t, err)

	var response map[string]any
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "pong", response["event"])
}

func TestWebSocketProxy_Reconnection(t *testing.T) {
	// Create a mock upstream server that maintains the connection
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		// Keep the connection alive by reading messages
		for {
			_, _, err := conn.Read(context.Background())
			if err != nil {
				return
			}
		}
	}))
	defer upstreamServer.Close()

	// Convert http URL to ws URL for upstream
	upstreamURL := "ws" + strings.TrimPrefix(upstreamServer.URL, "http")

	config := &Config{}
	proxy := NewWebSocketProxy(upstreamURL, config)

	// Start the proxy
	go proxy.Start()

	// Wait for initial connection
	time.Sleep(100 * time.Millisecond)
	require.NotNil(t, proxy.upstream)

	// Store the old connection
	oldConn := proxy.upstream

	// Simulate upstream connection failure
	oldConn.CloseNow()

	// Wait for reconnection with timeout
	timeout := time.After(2 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

waitLoop:
	for {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for reconnection")
		case <-ticker.C:
			if proxy.upstream != nil && proxy.upstream != oldConn {
				break waitLoop
			}
		}
	}
}

func TestWebSocketProxy_InvalidMessage(t *testing.T) {
	config := &Config{}
	proxy := NewWebSocketProxy("ws://example.com", config)

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Connect client
	client, _, err := websocket.Dial(context.Background(), wsURL, nil)
	require.NoError(t, err)
	defer client.CloseNow()

	// Test case 1: Invalid JSON message
	invalidMsg := []byte("invalid json")
	err = client.Write(context.Background(), websocket.MessageText, invalidMsg)
	require.NoError(t, err)

	// Verify connection remains open with ping
	pingMsg := map[string]any{
		"op": "ping",
	}
	pingBytes, _ := json.Marshal(pingMsg)
	err = client.Write(context.Background(), websocket.MessageText, pingBytes)
	require.NoError(t, err)

	// Read pong response
	_, message, err := client.Read(context.Background())
	require.NoError(t, err)
	var response map[string]any
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "pong", response["event"])

	// Test case 2: Malformed subscription message (args not an array)
	malformedMsg := map[string]any{
		"op":      "subscribe",
		"channel": "swap.update",
		"args":    "not-an-array", // Should be an array
	}
	malformedBytes, _ := json.Marshal(malformedMsg)
	err = client.Write(context.Background(), websocket.MessageText, malformedBytes)
	require.NoError(t, err)

	// Verify connection remains open with ping
	err = client.Write(context.Background(), websocket.MessageText, pingBytes)
	require.NoError(t, err)

	// Read pong response
	_, message, err = client.Read(context.Background())
	require.NoError(t, err)
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "pong", response["event"])
}

func TestWebSocketProxy_SubscriptionManagement(t *testing.T) {
	config := &Config{}
	proxy := NewWebSocketProxy("ws://example.com", config)

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Connect first client
	client1, _, err := websocket.Dial(context.Background(), wsURL, nil)
	require.NoError(t, err)
	defer client1.CloseNow()

	// Connect second client
	client2, _, err := websocket.Dial(context.Background(), wsURL, nil)
	require.NoError(t, err)
	defer client2.CloseNow()

	// Subscribe client1 to a swap
	swapID := "swap123"
	subscribeMsg := map[string]any{
		"op":      "subscribe",
		"channel": "swap.update",
		"args":    []any{swapID},
	}
	subscribeBytes, _ := json.Marshal(subscribeMsg)
	err = client1.Write(context.Background(), websocket.MessageText, subscribeBytes)
	require.NoError(t, err)

	// Read subscribe response
	_, message, err := client1.Read(context.Background())
	require.NoError(t, err)
	var response map[string]any
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "subscribe", response["event"])

	// Verify subscription state
	proxy.mu.Lock()
	// Get the server connection for client1
	var serverConn1 *websocket.Conn
	for conn, swaps := range proxy.clients {
		if len(swaps) > 0 {
			serverConn1 = conn
			break
		}
	}
	require.NotNil(t, serverConn1, "Server connection for client1 not found")
	assert.True(t, proxy.clients[serverConn1][swapID])
	assert.True(t, proxy.subscribers[swapID][serverConn1])
	assert.Equal(t, "swap.update", proxy.subscriptions[swapID].channel)
	proxy.mu.Unlock()

	// Subscribe client2 to the same swap
	err = client2.Write(context.Background(), websocket.MessageText, subscribeBytes)
	require.NoError(t, err)

	// Read subscribe response
	_, message, err = client2.Read(context.Background())
	require.NoError(t, err)
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "subscribe", response["event"])

	// Verify both clients are subscribed
	proxy.mu.Lock()
	// Get the server connection for client2
	var serverConn2 *websocket.Conn
	for conn, swaps := range proxy.clients {
		if conn != serverConn1 && len(swaps) > 0 {
			serverConn2 = conn
			break
		}
	}
	require.NotNil(t, serverConn2, "Server connection for client2 not found")
	assert.True(t, proxy.clients[serverConn1][swapID])
	assert.True(t, proxy.clients[serverConn2][swapID])
	assert.True(t, proxy.subscribers[swapID][serverConn1])
	assert.True(t, proxy.subscribers[swapID][serverConn2])
	assert.Equal(t, 2, len(proxy.subscribers[swapID]))
	proxy.mu.Unlock()

	// Unsubscribe client1
	unsubscribeMsg := map[string]any{
		"op":      "unsubscribe",
		"channel": "swap.update",
		"args":    []any{swapID},
	}
	unsubscribeBytes, _ := json.Marshal(unsubscribeMsg)
	err = client1.Write(context.Background(), websocket.MessageText, unsubscribeBytes)
	require.NoError(t, err)

	// Read unsubscribe response
	_, message, err = client1.Read(context.Background())
	require.NoError(t, err)
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "unsubscribe", response["event"])

	// Verify client1 is unsubscribed but client2 remains
	proxy.mu.Lock()
	assert.False(t, proxy.clients[serverConn1][swapID])
	assert.True(t, proxy.clients[serverConn2][swapID])
	assert.False(t, proxy.subscribers[swapID][serverConn1])
	assert.True(t, proxy.subscribers[swapID][serverConn2])
	assert.Equal(t, 1, len(proxy.subscribers[swapID]))
	proxy.mu.Unlock()

	// Unsubscribe client2
	err = client2.Write(context.Background(), websocket.MessageText, unsubscribeBytes)
	require.NoError(t, err)

	// Read unsubscribe response
	_, message, err = client2.Read(context.Background())
	require.NoError(t, err)
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "unsubscribe", response["event"])

	// Verify all subscriptions are cleaned up
	proxy.mu.Lock()
	assert.False(t, proxy.clients[serverConn1][swapID])
	assert.False(t, proxy.clients[serverConn2][swapID])
	assert.Nil(t, proxy.subscribers[swapID])
	assert.Nil(t, proxy.subscriptions[swapID])
	proxy.mu.Unlock()
}

func TestWebSocketProxy_MultipleSubscriptions(t *testing.T) {
	config := &Config{}
	proxy := NewWebSocketProxy("ws://example.com", config)

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Connect client
	client, _, err := websocket.Dial(context.Background(), wsURL, nil)
	require.NoError(t, err)
	defer client.CloseNow()

	// Subscribe to multiple swaps
	swapIDs := []string{"swap1", "swap2", "swap3"}
	for _, swapID := range swapIDs {
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
	}

	// Verify all subscriptions
	proxy.mu.Lock()
	// Get the server connection
	var serverConn *websocket.Conn
	for conn, swaps := range proxy.clients {
		if len(swaps) > 0 {
			serverConn = conn
			break
		}
	}
	require.NotNil(t, serverConn, "Server connection not found")
	for _, swapID := range swapIDs {
		assert.True(t, proxy.clients[serverConn][swapID])
		assert.True(t, proxy.subscribers[swapID][serverConn])
		assert.Equal(t, "swap.update", proxy.subscriptions[swapID].channel)
	}
	proxy.mu.Unlock()

	// Unsubscribe from all swaps at once
	unsubscribeMsg := map[string]any{
		"op":      "unsubscribe",
		"channel": "swap.update",
		"args":    swapIDs,
	}
	unsubscribeBytes, _ := json.Marshal(unsubscribeMsg)
	err = client.Write(context.Background(), websocket.MessageText, unsubscribeBytes)
	require.NoError(t, err)

	// Read unsubscribe response
	_, message, err := client.Read(context.Background())
	require.NoError(t, err)
	var response map[string]any
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "unsubscribe", response["event"])

	// Verify all subscriptions are cleaned up
	proxy.mu.Lock()
	for _, swapID := range swapIDs {
		assert.False(t, proxy.clients[serverConn][swapID])
		assert.Nil(t, proxy.subscribers[swapID])
		assert.Nil(t, proxy.subscriptions[swapID])
	}
	proxy.mu.Unlock()
}

func TestWebSocketProxy_ClientDisconnection(t *testing.T) {
	config := &Config{}
	proxy := NewWebSocketProxy("ws://example.com", config)

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Connect client
	client, _, err := websocket.Dial(context.Background(), wsURL, nil)
	require.NoError(t, err)

	// Subscribe to multiple swaps
	swapIDs := []string{"swap1", "swap2", "swap3"}
	for _, swapID := range swapIDs {
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
	}

	// Verify all subscriptions
	proxy.mu.Lock()
	// Get the server connection
	var serverConn *websocket.Conn
	for conn, swaps := range proxy.clients {
		if len(swaps) > 0 {
			serverConn = conn
			break
		}
	}
	require.NotNil(t, serverConn, "Server connection not found")
	for _, swapID := range swapIDs {
		assert.True(t, proxy.clients[serverConn][swapID])
		assert.True(t, proxy.subscribers[swapID][serverConn])
	}
	proxy.mu.Unlock()

	// Close client connection
	client.CloseNow()

	// Wait for cleanup
	time.Sleep(100 * time.Millisecond)

	// Verify all subscriptions are cleaned up
	proxy.mu.Lock()
	assert.Nil(t, proxy.clients[serverConn])
	for _, swapID := range swapIDs {
		assert.Nil(t, proxy.subscribers[swapID])
		assert.Nil(t, proxy.subscriptions[swapID])
	}
	proxy.mu.Unlock()
}

func TestWebSocketProxy_OfferManagement(t *testing.T) {
	config := &Config{}
	proxy := NewWebSocketProxy("ws://example.com", config)

	// Create a test server for the proxy
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Connect client
	client, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()

	// Subscribe to an offer
	subscribeMsg := map[string]any{
		"op":      "subscribe",
		"channel": "invoice.request",
		"args": []any{
			map[string]any{
				"offer": "test-offer",
			},
		},
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

	// Verify subscription state
	proxy.mu.Lock()
	var serverConn *websocket.Conn
	for conn, offers := range proxy.clients {
		if len(offers) > 0 {
			serverConn = conn
			break
		}
	}
	require.NotNil(t, serverConn, "Server connection not found")
	assert.True(t, proxy.clients[serverConn]["test-offer"])
	assert.True(t, proxy.subscribers["test-offer"][serverConn])
	assert.Equal(t, "invoice.request", proxy.subscriptions["test-offer"].channel)
	proxy.mu.Unlock()

	// Unsubscribe
	unsubscribeMsg := map[string]any{
		"op":      "unsubscribe",
		"channel": "invoice.request",
		"args":    []string{"test-offer"},
	}
	unsubscribeBytes, _ := json.Marshal(unsubscribeMsg)
	err = client.Write(context.Background(), websocket.MessageText, unsubscribeBytes)
	require.NoError(t, err)

	// Read unsubscribe response
	_, message, err = client.Read(context.Background())
	require.NoError(t, err)
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "unsubscribe", response["event"])

	// Verify subscription is cleaned up
	proxy.mu.Lock()
	assert.False(t, proxy.clients[serverConn]["test-offer"])
	assert.Nil(t, proxy.subscribers["test-offer"])
	assert.Nil(t, proxy.subscriptions["test-offer"])
	proxy.mu.Unlock()
}

func TestWebSocketProxy_MultipleOfferSubscriptions(t *testing.T) {
	config := &Config{}
	proxy := NewWebSocketProxy("ws://example.com", config)

	// Create a test server for the proxy
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Connect client
	client, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()

	// Subscribe to multiple offers
	offerIDs := []string{"offer1", "offer2", "offer3"}
	for _, offerID := range offerIDs {
		subscribeMsg := map[string]any{
			"op":      "subscribe",
			"channel": "invoice.request",
			"args": []any{
				map[string]any{
					"offer": offerID,
				},
			},
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
	}

	// Verify all subscriptions
	proxy.mu.Lock()
	var serverConn *websocket.Conn
	for conn, offers := range proxy.clients {
		if len(offers) > 0 {
			serverConn = conn
			break
		}
	}
	require.NotNil(t, serverConn, "Server connection not found")
	for _, offerID := range offerIDs {
		assert.True(t, proxy.clients[serverConn][offerID])
		assert.True(t, proxy.subscribers[offerID][serverConn])
		assert.Equal(t, "invoice.request", proxy.subscriptions[offerID].channel)
	}
	proxy.mu.Unlock()

	// Unsubscribe from all offers at once
	unsubscribeMsg := map[string]any{
		"op":      "unsubscribe",
		"channel": "invoice.request",
		"args":    offerIDs,
	}
	unsubscribeBytes, _ := json.Marshal(unsubscribeMsg)
	err = client.Write(context.Background(), websocket.MessageText, unsubscribeBytes)
	require.NoError(t, err)

	// Read unsubscribe response
	_, message, err := client.Read(context.Background())
	require.NoError(t, err)
	var response map[string]any
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, "unsubscribe", response["event"])

	// Verify all subscriptions are cleaned up
	proxy.mu.Lock()
	for _, offerID := range offerIDs {
		assert.False(t, proxy.clients[serverConn][offerID])
		assert.Nil(t, proxy.subscribers[offerID])
		assert.Nil(t, proxy.subscriptions[offerID])
	}
	proxy.mu.Unlock()
}

func TestWebSocketProxy_MessageForwarding(t *testing.T) {
	// Create a mock upstream server that maintains connections and sends messages
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		// Keep the connection alive by reading messages
		go func() {
			for {
				_, _, err := conn.Read(context.Background())
				if err != nil {
					return
				}
			}
		}()

		// Send test messages with delays to prevent overwhelming the connection
		testCases := []map[string]any{
			{
				"event":   "update",
				"channel": "swap.update",
				"args": []any{
					map[string]any{
						"id":     "swap1",
						"status": "completed",
						"amount": 1000,
					},
				},
			},
			{
				"event":   "update",
				"channel": "swap.update",
				"args": []any{
					map[string]any{
						"id":     "swap2",
						"status": "failed",
						"error":  "insufficient funds",
					},
				},
			},
			{
				"event":   "update",
				"channel": "swap.update",
				"args": []any{
					map[string]any{
						"id":       "swap3",
						"status":   "pending",
						"progress": 50,
					},
				},
			},
		}

		for _, testCase := range testCases {
			messageBytes, _ := json.Marshal(testCase)
			err := conn.Write(context.Background(), websocket.MessageText, messageBytes)
			if err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer upstreamServer.Close()

	config := &Config{}
	proxy := NewWebSocketProxy("ws"+strings.TrimPrefix(upstreamServer.URL, "http"), config)

	// Start proxy with a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		proxy.Start()
		<-ctx.Done()
	}()
	defer cancel()

	// Wait for initial connection
	time.Sleep(500 * time.Millisecond)

	// Create test server for proxy
	server := httptest.NewServer(http.HandlerFunc(proxy.HandleWebSocket))
	defer server.Close()

	// Connect client
	client, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()

	// Subscribe to all test swaps
	subscribeMsg := map[string]any{
		"op":      "subscribe",
		"channel": "swap.update",
		"args":    []any{"swap1", "swap2", "swap3"},
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

	// Verify all update messages are received correctly
	receivedUpdates := make(map[string]map[string]any)
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, message, err := client.Read(ctx)
		cancel()
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
		id, ok := swapStatus["id"].(string)
		require.True(t, ok)
		receivedUpdates[id] = swapStatus
	}

	// Verify we received all expected updates with correct content
	assert.Equal(t, "completed", receivedUpdates["swap1"]["status"])
	assert.Equal(t, float64(1000), receivedUpdates["swap1"]["amount"])

	assert.Equal(t, "failed", receivedUpdates["swap2"]["status"])
	assert.Equal(t, "insufficient funds", receivedUpdates["swap2"]["error"])

	assert.Equal(t, "pending", receivedUpdates["swap3"]["status"])
	assert.Equal(t, float64(50), receivedUpdates["swap3"]["progress"])
}
