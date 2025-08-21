package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"sabre/internal/backend"
	"sabre/internal/router"

	"github.com/dustin/go-humanize"
	"github.com/joho/godotenv"
)

// Version and build information - set by linker flags
var (
	Version   = "dev"
	BuildTime = "unknown"
)

var (
	tickEvery = time.Second
	barMax    = 60
	glyphRPS  = 10.0
	emaAlpha  = 0.35
)

func animate() func() {
	tick := time.NewTicker(tickEvery)
	done := make(chan struct{})

	go func() {
		defer tick.Stop()
		var last uint64
		var ema float64
		for {
			select {
			case <-tick.C:
				cur := router.TotalReq.Load()
				delta := float64(cur - last)
				last = cur

				rps := delta / tickEvery.Seconds()
				if ema == 0 {
					ema = rps
				} else {
					ema = emaAlpha*rps + (1-emaAlpha)*ema
				}
				n := min(max(int(ema/glyphRPS+0.5), 4), barMax)
				bar := "<----]=╦" + strings.Repeat("═", n) + "▷"
				fmt.Fprintf(os.Stdout, "\r%-80s %s rps %s total", bar, humanize.SI(rps, ""), humanize.SI(float64(cur), ""))
			case <-done:
				return
			}
		}
	}()

	return func() { close(done); fmt.Fprintln(os.Stdout) }
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	
	// Add version flag
	showVersion := flag.Bool("version", false, "show version information")
	cfgPath := flag.String("c", "config.toml", "path to config.toml file")
	envPath := flag.String("e", ".env", "path to .env file")
	flag.Parse()
	
	// Show version and exit if requested
	if *showVersion {
		fmt.Printf("sabre version %s (built %s)\n", Version, BuildTime)
		os.Exit(0)
	}

	if _, err := os.Stat(*envPath); err == nil {
		if err := godotenv.Load(*envPath); err != nil {
			log.Fatalf("loading .env file: %v", err)
		}
	}

	cfg := backend.ParseConfig(*cfgPath)
	lb := backend.NewLoadBalancer(cfg)
	if cfg.HasWebSocket {
		cfg.Stream = backend.NewStream(cfg, lb)
	}

	cstore, err := backend.Open(cfg.Cache)
	if err != nil {
		log.Fatalf("cache open: %v", err)
	}
	defer cstore.Close()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	server := router.NewRouter(cstore, &cfg, lb)

	go func() {
		<-sigChan

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		if cfg.BatchProcessor != nil {
			cfg.BatchProcessor.FlushAll()
		}

		if cfg.Stream != nil {
			cfg.Stream.Shutdown(shutdownCtx)
		}

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Error during server shutdown: %v", err)
		}
	}()

	stop := animate()
	defer stop()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
