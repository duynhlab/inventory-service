package database

import (
	"context"
	"testing"
	"time"

	"github.com/duynhlab/inventory-service/config"
)

func TestConnect_ParseError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Database = config.DatabaseConfig{
		Host:           "localhost",
		Port:           "5432",
		Name:           "inventory",
		User:           "inventory",
		Password:       "secret",
		SSLMode:        "bogus",
		MaxConnections: 25,
	}
	if _, err := Connect(context.Background(), cfg); err == nil {
		t.Fatal("want parse error")
	}
}

func TestConnect_PingError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Database = config.DatabaseConfig{
		Host:           "127.0.0.1",
		Port:           "1",
		Name:           "inventory",
		User:           "inventory",
		Password:       "secret",
		SSLMode:        "disable",
		MaxConnections: 25,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := Connect(ctx, cfg); err == nil {
		t.Fatal("want ping error")
	}
}
