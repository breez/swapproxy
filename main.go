package main

import (
	"log"
	"net/http"
)

func main() {
	log.Printf("Starting proxy server...")

	config, err := LoadConfig()
	if err != nil {
		log.Fatal("Error loading config:", err)
	}
	log.Printf("Loaded configuration - Backend URL: %s, Port: %s, DB Path: %s",
		config.BackendURL.String(),
		config.Port,
		config.DBPath)

	db, err := InitDatabase(config.DBPath)
	if err != nil {
		log.Fatal("Error initializing database:", err)
	}
	defer db.Close()
	log.Printf("Database initialized successfully")

	// Set up HTTP reverse proxy
	proxy := NewReverseProxy(config, db)
	log.Printf("Reverse proxy created")

	// Set up WebSocket proxy
	wsProxy := NewWebSocketProxy(config.WebSocketBackendURL.String(), config)
	wsProxy.Start()
	http.HandleFunc("/ws", wsProxy.HandleWebSocket)

	// Set up HTTP handler
	http.Handle("/", proxy)

	log.Printf("Starting to listen on port %s", config.Port)
	log.Fatal(http.ListenAndServe(":"+config.Port, nil))
}
