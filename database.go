package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type requestLog struct {
	Timestamp    time.Time
	Method       string
	URI          string
	Status       int
	RequestBody  string
	ResponseBody string
	APIKey       string
}

type customTransport struct {
	base http.RoundTripper
}

func (t *customTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Check if request is unauthorized
	if unauthorized, ok := req.Context().Value("unauthorized").(bool); ok && unauthorized {
		// Return unauthorized response without forwarding to backend
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(bytes.NewBufferString("Invalid")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}

	// Forward request to backend only if authorized
	return t.base.RoundTrip(req)
}

func InitDatabase(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("error opening database: %w", err)
	}

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS requests (
            timestamp DATETIME,
            method TEXT,
            uri TEXT,
            status INTEGER,
            request_body TEXT,
            response_body TEXT,
            api_key TEXT
        )
    `)
	if err != nil {
		return nil, fmt.Errorf("error creating table: %w", err)
	}
	return db, nil
}

func validateAPIKey(cert *x509.Certificate, apiKeyStr string) bool {
	apiKeyBytes, err := base64.StdEncoding.DecodeString(apiKeyStr)
	if err != nil {
		log.Printf("Error decoding API key: %v", err)
		return false
	}

	apiKeyCert, err := x509.ParseCertificate(apiKeyBytes)
	if err != nil {
		log.Printf("Error parsing API key certificate: %v", err)
		return false
	}

	certPool := x509.NewCertPool()
	certPool.AddCert(cert)

	_, err = apiKeyCert.Verify(x509.VerifyOptions{
		Roots: certPool,
	})

	if err != nil {
		log.Printf("Certificate verification failed: %v", err)
		return false
	}

	return true
}

func NewReverseProxy(config *Config, db *sql.DB) *httputil.ReverseProxy {
	log.Printf("Initializing reverse proxy with backend URL: %s (scheme: %s, host: %s)",
		config.BackendURL.String(),
		config.BackendURL.Scheme,
		config.BackendURL.Host)

	proxy := httputil.NewSingleHostReverseProxy(config.BackendURL)

	// Set up custom transport
	proxy.Transport = &customTransport{
		base: http.DefaultTransport,
	}

	var reqBody []byte

	proxy.Director = func(req *http.Request) {
		log.Printf("Incoming request: %s %s", req.Method, req.URL.String())

		ctx := context.WithValue(req.Context(), "start", time.Now())
		*req = *req.WithContext(ctx)

		// Store original values
		originalPath := req.URL.Path
		originalQuery := req.URL.RawQuery
		log.Printf("Original path: %s, query: %s", originalPath, originalQuery)

		// Always set up the URL properly, regardless of authentication
		newURL, err := url.Parse(config.BackendURL.String())
		if err != nil {
			log.Printf("Error parsing backend URL: %v", err)
			return
		}

		// Set the path and query
		newURL.Path = path.Join(newURL.Path, originalPath)
		newURL.RawQuery = originalQuery

		// Update the request URL
		req.URL = newURL
		req.Host = newURL.Host

		log.Printf("Set up request URL: %s", req.URL.String())

		// Now handle authentication
		authHeader := req.Header.Get("Authorization")
		log.Printf("Authorization header: %s", authHeader)

		apiKey := ""
		if strings.HasPrefix(authHeader, "Bearer ") {
			apiKey = strings.TrimPrefix(authHeader, "Bearer ")
			log.Printf("Extracted API key (length): %d", len(apiKey))
		} else {
			log.Printf("No Bearer token found in Authorization header")
		}

		// Remove Authorization header before forwarding
		req.Header.Del("Authorization")

		if apiKey == "" || !validateAPIKey(config.CACert, apiKey) {
			log.Printf("API key validation failed")
			ctx = context.WithValue(req.Context(), "unauthorized", true)
			*req = *req.WithContext(ctx)
			return
		}

		ctx = context.WithValue(req.Context(), "api_key", apiKey)
		*req = *req.WithContext(ctx)

		if req.Body != nil {
			var err error
			reqBody, err = io.ReadAll(req.Body)
			if err != nil {
				log.Printf("Error reading request body: %v", err)
			}
			req.Body = io.NopCloser(bytes.NewBuffer(reqBody))
			log.Printf("Request body length: %d", len(reqBody))
		}

		log.Printf("Final request URL: %s", req.URL.String())
		log.Printf("Final request details - Scheme: %s, Host: %s, Path: %s",
			req.URL.Scheme,
			req.URL.Host,
			req.URL.Path)
	}

	proxy.ModifyResponse = func(res *http.Response) error {
		elapsed := time.Since(res.Request.Context().Value("start").(time.Time))
		log.Printf("Received response with status: %d after %v", res.StatusCode, elapsed)

		if unauthorized, ok := res.Request.Context().Value("unauthorized").(bool); ok && unauthorized {
			log.Printf("Request was unauthorized")
			return nil
		}

		if res == nil {
			log.Println("Error: nil response")
			return fmt.Errorf("nil response")
		}

		var resBody []byte
		if res.Body != nil {
			resBody, _ = io.ReadAll(res.Body)
			res.Body = io.NopCloser(bytes.NewBuffer(resBody))
			log.Printf("Response body length: %d", len(resBody))
		}

		LogRequest(db, res.Request, res, elapsed, string(reqBody), string(resBody))
		return nil
	}

	return proxy
}

func LogRequest(db *sql.DB, req *http.Request, res *http.Response, elapsed time.Duration, requestBody, responseBody string) {
	if res == nil {
		log.Printf("Error: nil response, using default 500 status code for logging.")
		res = &http.Response{StatusCode: http.StatusInternalServerError}
	}

	apiKey := ""
	if key, ok := req.Context().Value("api_key").(string); ok {
		apiKey = key
	}

	_, err := db.Exec(`
        INSERT INTO requests (timestamp, method, uri, status, request_body, response_body, api_key)
        VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now(), req.Method, req.URL.String(), res.StatusCode, requestBody, responseBody, apiKey)

	if err != nil {
		log.Printf("Error logging request: %s", err)
	}

	log.Printf("%s %s %d %s API Key: %s Request Body: %s, Response Body: %s",
		req.Method, req.URL.String(), res.StatusCode, elapsed, apiKey, requestBody, responseBody)
}
