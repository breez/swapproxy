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
		config.SQLiteDBPath)

	sqlite, err := InitSQLiteDatabase(config.SQLiteDBPath)
	if err != nil {
		log.Fatal("Error initializing SQLite database:", err)
	}
	defer sqlite.Close()
	log.Printf("SQLite Database initialized successfully")

	postgres, err := InitPostgresDatabase(config.PostgresURL)
	if err != nil {
		log.Fatal("Error initializing Postgres database:", err)
	}
	defer postgres.Close()
	log.Printf("Postgres Database connection initialized successfully")

	// Set up HTTP reverse proxy
	proxy := NewReverseProxy(config, sqlite, postgres)
	log.Printf("Reverse proxy created")

	// Set up WebSocket proxy
	wsProxy := NewWebSocketProxy(config.WebSocketBackendURL.String(), config)
	wsProxy.Start()
	http.HandleFunc("/v2/ws", wsProxy.HandleWebSocket)

	// Set up HTTP handler
	http.Handle("/", proxy)

	log.Printf("Starting to listen on port %s", config.Port)
	log.Fatal(http.ListenAndServe(":"+config.Port, nil))
}
