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

	proxy := NewReverseProxy(config, db)
	log.Printf("Reverse proxy created")

	log.Printf("Starting to listen on port %s", config.Port)
	log.Fatal(http.ListenAndServe(":"+config.Port, proxy))
}
