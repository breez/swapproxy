package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Database struct {
	Pool *pgxpool.Pool
}

func InitPostgresDatabase(databaseURL string) (*Database, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &Database{Pool: pool}, nil
}

func (db *Database) Close() {
	db.Pool.Close()
}

type Fee struct {
	PartnerID     int
	FeePercentage float64
}

type FeeModel struct {
	DB *pgxpool.Pool
}

func NewFeeModel(db *pgxpool.Pool) *FeeModel {
	return &FeeModel{DB: db}
}

func (m *FeeModel) GetByPartnerApiKey(api_key string) (*Fee, error) {
	query := `SELECT p.id, fs.fee_percentage
				FROM partners p
				JOIN fee_settings fs ON p.id = fs.partner_id
				WHERE p.api_key = $1;`
	row := m.DB.QueryRow(context.Background(), query, api_key)

	var fee Fee
	err := row.Scan(&fee.PartnerID, &fee.FeePercentage)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	return &fee, nil
}
