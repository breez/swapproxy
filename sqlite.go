package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
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

func InitSQLiteDatabase(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("error opening SQLite database: %w", err)
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
