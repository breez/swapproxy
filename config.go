package main

import (
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	BackendURL *url.URL
	Port       string
	DBPath     string
	CACert     *x509.Certificate
}

func LoadConfig() (*Config, error) {
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file, using defaults")
	}

	backendURLStr := os.Getenv("BACKEND_URL")
	if backendURLStr == "" {
		backendURLStr = "http://localhost:8081"
	}
	backendURL, err := url.Parse(backendURLStr)
	if err != nil {
		return nil, fmt.Errorf("invalid BACKEND_URL: %w", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "requests.db"
	}

	caCertPEM := os.Getenv("CA_CERT")
	if caCertPEM == "" {
		return nil, fmt.Errorf("CA_CERT environment variable is required")
	}

	block, err := base64.StdEncoding.DecodeString(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("Could not decode certificate base64 body: %w", err)
	}

	caCert, err := x509.ParseCertificate(block)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	return &Config{
		BackendURL: backendURL,
		Port:       port,
		DBPath:     dbPath,
		CACert:     caCert,
	}, nil
}
