package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"rapier/internal/backend"
	"rapier/internal/router"

	"github.com/joho/godotenv"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	cfgPath := flag.String("c", "config.toml", "path to config.toml file")
	envPath := flag.String("e", ".env", "path to .env file")
	flag.Parse()

	if _, err := os.Stat(*envPath); err == nil {
		if err := godotenv.Load(*envPath); err != nil {
			log.Fatalf("loading .env file: %v", err)
		}
	}

	cfg := backend.ParseConfig(*cfgPath)
	lb := backend.NewLoadBalancer(cfg)
	cstore, err := backend.Open(cfg.Cache)
	if err != nil {
		log.Fatalf("cache open: %v", err)
	}
	defer cstore.Close()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	server := router.NewRouter(cstore, &cfg, lb)

	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v, initiating graceful shutdown...", sig)

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		if cfg.BatchProcessor != nil {
			log.Println("Flushing pending batches...")
			cfg.BatchProcessor.FlushAll()
		}

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Error during server shutdown: %v", err)
		}
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	log.Println("Server shutdown complete")
}
