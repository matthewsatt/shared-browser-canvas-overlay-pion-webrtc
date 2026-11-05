package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	collab "github.com/example/collab-browser"
	"github.com/example/collab-browser/internal/api"
	"github.com/example/collab-browser/internal/config"
	"github.com/example/collab-browser/internal/session"
)

func main() {
	// `collab-browser healthcheck`: used by Docker's HEALTHCHECK (the image
	// has no curl). Exits 0 if the server answers /healthz.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.AllowPrivateNetworks {
		log.Printf("WARNING: ALLOW_PRIVATE_NETWORKS=true — the shared browser can reach internal addresses. Development only.")
	}
	if cfg.APIToken == "" {
		log.Printf("WARNING: API_TOKEN is not set — anyone who can reach this server can start sessions. Set it in production.")
	}
	if cfg.ChromeNoSandbox {
		log.Printf("WARNING: CHROME_NO_SANDBOX=true — Chromium's sandbox is off, so a compromised page could reach other sessions. Development only.")
	}
	if len(cfg.AllowedURLPrefixes) == 0 {
		log.Printf("note: ALLOWED_URL_PREFIXES is not set — sessions may open any public page.")
	}
	mgr, err := session.NewManager(cfg)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.New(cfg, mgr, collab.WebFS()).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	mgr.Shutdown()
}

func healthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
