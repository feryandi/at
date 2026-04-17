package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/ferya/at/internal/api"
	"github.com/ferya/at/internal/deploy"
	"github.com/ferya/at/internal/proxy"
	"github.com/ferya/at/internal/store"
)

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Printf("warning: invalid %s=%q, using default %d", key, v, defaultVal)
			return defaultVal
		}
		return n
	}
	return defaultVal
}

func main() {
	dataDir        := getEnv("AT_DATA_DIR", "./data")
	projectsDir    := getEnv("AT_PROJECTS_DIR", "./projects")
	port           := getEnv("AT_PORT", "8080")
	caddyAdmin     := getEnv("AT_CADDY_ADMIN", "http://localhost:2019")
	portRangeStart := getEnvInt("AT_PORT_RANGE_START", 10000)
	baseDomain     := getEnv("AT_BASE_DOMAIN", "")
	upstreamHost   := getEnv("AT_UPSTREAM_HOST", "localhost")

	// Ensure data and projects directories exist
	for _, dir := range []string{dataDir, projectsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("create dir %s: %v", dir, err)
		}
	}

	// Init SQLite store
	dbPath := filepath.Join(dataDir, "at.db")
	db, err := store.New(dbPath, portRangeStart)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}
	defer db.Close()
	log.Printf("store: opened %s", dbPath)

	// Init Docker client
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("init docker client: %v", err)
	}
	defer dockerClient.Close()
	log.Printf("docker: client initialized")

	// Init Caddy proxy
	caddy := proxy.NewCaddy(caddyAdmin)
	log.Printf("caddy: admin URL = %s", caddyAdmin)

	// Init pipeline
	pipeline := deploy.NewPipeline(db, dockerClient, caddy, projectsDir, baseDomain, upstreamHost)
	log.Printf("projects: scanning %s", projectsDir)

	// Scan projects directory for new apps
	if err := pipeline.ScanProjects(); err != nil {
		log.Printf("scan warning: %v", err)
	}

	// Reconcile running containers on startup
	log.Printf("reconcile: checking running containers...")
	if err := pipeline.ReconcileRunning(context.Background()); err != nil {
		log.Printf("reconcile warning: %v", err)
	}

	// Set up HTTP server
	mux := http.NewServeMux()
	handler := api.NewHandler(db, pipeline, caddy, baseDomain)
	handler.RegisterRoutes(mux)

	addr := fmt.Sprintf(":%s", port)
	log.Printf("server: listening on %s", addr)
	log.Printf("dashboard: http://localhost:%s", port)

	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("server: shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("server: forced shutdown: %v", err)
	}
	log.Println("server: stopped")
}
