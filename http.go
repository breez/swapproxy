package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

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

type ExtraFees struct {
	ID         string  `json:"id"`
	Percentage float64 `json:"percentage"`
}

func addRequestExtraFees(body []byte, extraFee Fee) ([]byte, error) {
	var jsonBody map[string]interface{}
	if err := json.Unmarshal(body, &jsonBody); err != nil {
		return nil, fmt.Errorf("error unmarshaling JSON: %w", err)
	}

	jsonBody["extraFees"] = ExtraFees{
		ID:         strconv.Itoa(extraFee.PartnerID),
		Percentage: extraFee.FeePercentage,
	}

	modifiedBody, err := json.Marshal(jsonBody)
	if err != nil {
		return nil, fmt.Errorf("error marshaling JSON: %w", err)
	}

	return modifiedBody, nil
}

func modifyResponsePercentages(data interface{}, extraFeePercentage float64) interface{} {
	// The response is a map of currency pairs
	currencyMap, ok := data.(map[string]interface{})
	if !ok {
		return data
	}

	// Iterate through each currency pair
	for _, pairData := range currencyMap {
		pairMap, ok := pairData.(map[string]interface{})
		if !ok {
			continue
		}

		// Each pair has another map of currency data
		for _, currencyData := range pairMap {
			currencyMap, ok := currencyData.(map[string]interface{})
			if !ok {
				continue
			}

			// Check for fees.percentage
			if fees, ok := currencyMap["fees"].(map[string]interface{}); ok {
				if percentage, ok := fees["percentage"].(float64); ok {
					fees["percentage"] = percentage + extraFeePercentage
				}
			}
		}
	}

	return data
}

func getExtraFee(postgres *Database, apiKey string) *Fee {
	fee, err := NewFeeModel(postgres.Pool).GetByPartnerApiKey(apiKey)
	if err != nil {
		log.Printf("Error getting extra fee from database: %v", err)
		// TODO: should we fail?
	}
	return fee
}

func NewReverseProxy(config *Config, sqlite *sql.DB, postgres *Database) *httputil.ReverseProxy {
	log.Printf("Initializing reverse proxy with backend URL: %s (scheme: %s, host: %s)",
		config.BackendURL.String(),
		config.BackendURL.Scheme,
		config.BackendURL.Host)

	proxy := httputil.NewSingleHostReverseProxy(config.BackendURL)

	// Set up custom transport
	proxy.Transport = &customTransport{
		base: http.DefaultTransport,
	}

	proxy.Director = func(req *http.Request) {
		log.Printf("Incoming request: %s %s", req.Method, req.URL.String())

		ctx := context.WithValue(req.Context(), "start", time.Now())
		*req = *req.WithContext(ctx)

		// Store original values
		originalPath := req.URL.Path
		originalQuery := req.URL.Query()
		log.Printf("Original path: %s, query: %s", originalPath, originalQuery)

		for _, param := range config.HTTPAdditionalParams {
			if len(param) == 2 {
				originalQuery.Add(param[0], param[1])
			}
		}

		// Always set up the URL properly, regardless of authentication
		newURL, err := url.Parse(config.BackendURL.String())
		if err != nil {
			log.Printf("Error parsing backend URL: %v", err)
			return
		}

		// Set the path and query
		newURL.Path = path.Join(newURL.Path, originalPath)
		newURL.RawQuery = originalQuery.Encode()

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

		if config.CACert != nil {
			if apiKey == "" || !validateAPIKey(config.CACert, apiKey) {
				log.Printf("API key validation failed")
				ctx = context.WithValue(req.Context(), "unauthorized", true)
				*req = *req.WithContext(ctx)
				return
			}
		}

		// Check if this is a request we need to set extra fees for
		if req.Method == "POST" {
			switch originalPath {
			case "/v2/swap/submarine", "/v2/swap/reverse", "/v2/swap/chain":
				if req.Body != nil {
					reqBody, err := io.ReadAll(req.Body)
					if err != nil {
						log.Printf("Error reading request body: %v", err)
					} else {
						extraFee := getExtraFee(postgres, apiKey)
						if extraFee != nil {
							modifiedBody, err := addRequestExtraFees(reqBody, *extraFee)
							if err != nil {
								log.Printf("Error modifying request body: %v", err)
							} else {
								req.Body = io.NopCloser(bytes.NewBuffer(modifiedBody))
								req.ContentLength = int64(len(modifiedBody))
								log.Printf("Modified request body for %s", originalPath)
							}
						}
					}
				}
			}
		}

		ctx = context.WithValue(req.Context(), "api_key", apiKey)
		*req = *req.WithContext(ctx)

		if req.Body != nil {
			reqBody, err := io.ReadAll(req.Body)
			if err != nil {
				log.Printf("Error reading request body: %v", err)
			}
			req.Body = io.NopCloser(bytes.NewBuffer(reqBody))
			log.Printf("Request body length: %d", len(reqBody))
			ctx = context.WithValue(ctx, "request_body", reqBody)
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

		if res.StatusCode < 200 || res.StatusCode >= 300 {
			log.Printf("Received non-success status code: %d", res.StatusCode)
			return fmt.Errorf("Response with non-success status code: %d", res.StatusCode)
		}

		var resBody []byte
		if res.Body != nil {
			var err error
			resBody, err = io.ReadAll(res.Body)
			if err != nil {
				log.Printf("Error reading response body: %v", err)
				return err
			}
			res.Body.Close() // Close the original body

			// Check if this is a response where we need to modify fee percentages
			if res.Request.Method == "GET" {
				switch res.Request.URL.Path {
				case "/v2/swap/submarine", "/v2/swap/reverse", "/v2/swap/chain":
					var jsonBody interface{}
					if err := json.Unmarshal(resBody, &jsonBody); err != nil {
						log.Printf("Error unmarshaling response JSON: %v", err)
					} else {
						apiKey, _ := res.Request.Context().Value("api_key").(string)
						extraFee := getExtraFee(postgres, apiKey)
						if extraFee != nil {
							modifiedBody := modifyResponsePercentages(jsonBody, extraFee.FeePercentage)
							modifiedJSON, err := json.Marshal(modifiedBody)
							if err != nil {
								log.Printf("Error marshaling modified response JSON: %v", err)
							} else {
								resBody = modifiedJSON
								log.Printf("Modified percentage values in response for %s", res.Request.URL.Path)
							}
						}
					}
				}
			}

			// Create a new buffer and set it as the response body
			buf := bytes.NewBuffer(resBody)
			res.Body = io.NopCloser(buf)
			res.ContentLength = int64(buf.Len())
			res.Header.Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
			log.Printf("Response body length: %d", buf.Len())
		}

		reqBody := ""
		if body, ok := res.Request.Context().Value("request_body").([]byte); ok {
			reqBody = string(body)
		}
		LogRequest(sqlite, res.Request, res, elapsed, string(reqBody), string(resBody))
		return nil
	}

	return proxy
}
