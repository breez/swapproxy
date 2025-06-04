package main

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	BackendURL                *url.URL
	WebSocketBackendURL       *url.URL
	Port                      string
	DBPath                    string
	CACert                    *x509.Certificate
	HTTPAdditionalParams      [][2]string
	WebSocketAdditionalParams [][2]string
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

	webSocketBackendURLStr := os.Getenv("WEBSOCKET_BACKEND_URL")
	if backendURLStr == "" {
		webSocketBackendURLStr = "ws://localhost:8081"
	}
	webSocketBackendURL, err := url.Parse(webSocketBackendURLStr)
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

	var caCert *x509.Certificate
	if os.Getenv("DANGEROUS_NO_CA_CERT") != "YES" {
		caCertPEM := os.Getenv("CA_CERT")
		if caCertPEM == "" {
			return nil, fmt.Errorf("CA_CERT environment variable is required")
		}

		block, err := base64.StdEncoding.DecodeString(caCertPEM)
		if err != nil {
			return nil, fmt.Errorf("Could not decode certificate base64 body: %w", err)
		}

		caCert, err = x509.ParseCertificate(block)
		if err != nil {
			return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
		}
	}

	httpAdditionalParamsStr := os.Getenv("HTTP_ADDITIONAL_PARAMETERS")
	var httpAdditionalParams [][2]string
	if httpAdditionalParamsStr != "" {
		err := json.Unmarshal([]byte(httpAdditionalParamsStr), &httpAdditionalParams)
		if err != nil {
			return nil, fmt.Errorf("invalid HTTP_ADDITIONAL_PARAMETERS format: %w", err)
		}
	}

	websocketAdditionalParamsStr := os.Getenv("WEBSOCKET_ADDITIONAL_PARAMETERS")
	var websocketAdditionalParams [][2]string
	if websocketAdditionalParamsStr != "" {
		err := json.Unmarshal([]byte(websocketAdditionalParamsStr), &websocketAdditionalParams)
		if err != nil {
			return nil, fmt.Errorf("invalid WEBSOCKET_ADDITIONAL_PARAMETERS format: %w", err)
		}
	}

	return &Config{
		BackendURL:                backendURL,
		WebSocketBackendURL:       webSocketBackendURL,
		Port:                      port,
		DBPath:                    dbPath,
		CACert:                    caCert,
		HTTPAdditionalParams:      httpAdditionalParams,
		WebSocketAdditionalParams: websocketAdditionalParams,
	}, nil
}
