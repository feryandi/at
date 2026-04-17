package deploy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/ferya/at/internal/proxy"
	"github.com/ferya/at/internal/store"
	"github.com/google/uuid"
)

// validAppName matches Docker container naming rules: starts with alphanumeric,
// followed by alphanumerics, underscores, hyphens, or dots.
var validAppName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.\-]*$`)

// Pipeline manages the build and deploy lifecycle for apps.
type Pipeline struct {
	db           *store.DB
	docker       *client.Client
	caddy        *proxy.Caddy
	projectsDir  string
	baseDomain   string
	upstreamHost string // host Caddy uses to reach app containers, e.g. "host.docker.internal"
	locks        sync.Map // per-app semaphore: map[appID]chan struct{}
}

// NewPipeline creates a new deployment pipeline.
func NewPipeline(db *store.DB, docker *client.Client, caddy *proxy.Caddy, projectsDir, baseDomain, upstreamHost string) *Pipeline {
	if upstreamHost == "" {
		upstreamHost = "localhost"
	}
	return &Pipeline{
		db:           db,
		docker:       docker,
		caddy:        caddy,
		projectsDir:  projectsDir,
		baseDomain:   baseDomain,
		upstreamHost: upstreamHost,
	}
}

// Deploy triggers an async deployment. Returns (deploymentID, error).
// Returns error if a deploy is already in progress for this app.
func (p *Pipeline) Deploy(appID string) (string, error) {
	app, err := p.db.GetApp(appID)
	if err != nil {
		return "", fmt.Errorf("get app: %w", err)
	}
	if app == nil {
		return "", fmt.Errorf("app not found: %s", appID)
	}

	unlock, ok := p.tryLock(appID)
	if !ok {
		return "", fmt.Errorf("deployment already in progress for app %s", app.Name)
	}

	depID := uuid.New().String()
	dep := &store.Deployment{
		ID:        depID,
		AppID:     appID,
		Status:    store.StatusPending,
		StartedAt: time.Now().UTC(),
	}
	if err := p.db.CreateDeployment(dep); err != nil {
		unlock()
		return "", fmt.Errorf("create deployment record: %w", err)
	}

	go func() {
		defer unlock()
		if err := p.runDeploy(context.Background(), app, dep); err != nil {
			log.Printf("[deploy] app=%s dep=%s error: %v", app.Name, depID, err)
			_ = p.db.UpdateDeploymentStatus(depID, store.StatusFailed, err.Error())
		}
	}()

	return depID, nil
}

// runDeploy executes the full deploy flow synchronously.
func (p *Pipeline) runDeploy(ctx context.Context, app *store.App, dep *store.Deployment) error {
	app.Name = strings.ToLower(app.Name)
	depID := dep.ID
	appID := app.ID

	logFn := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		line := fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05"), msg)
		log.Printf("[deploy:%s] %s", app.Name, msg)
		_ = p.db.AppendDeploymentLog(depID, line)
	}

	// 1. Set status = building
	_ = p.db.UpdateDeploymentStatus(depID, store.StatusBuilding, "")
	logFn("==> Starting deployment %s", depID)

	// 2. Locate project directory
	projectDir := filepath.Join(p.projectsDir, app.Name)
	if _, err := os.Stat(projectDir); os.IsNotExist(err) {
		return fmt.Errorf("project directory not found: %s", projectDir)
	}
	logFn("==> Building from %s", projectDir)

	// 3. Build Docker image
	imageTag := fmt.Sprintf("at-%s:%s", app.Name, depID[:8])
	logFn("==> Building Docker image %s...", imageTag)
	if err := runCmdInDir(projectDir, logFn, "docker", "build",
		"-t", imageTag,
		"--label", fmt.Sprintf("at.app=%s", app.Name),
		"-f", "Dockerfile",
		".",
	); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	logFn("==> Build complete: %s", imageTag)

	// 4. Container name (consistent, enforces single instance)
	containerName := fmt.Sprintf("at-%s", app.Name)

	// Remember previous running deployment to mark as stopped later
	prevDep, _ := p.db.GetLatestDeployment(appID)
	// Don't count this deployment as "previous"
	if prevDep != nil && prevDep.ID == depID {
		prevDep = nil
	}

	// 5. Stop old container
	logFn("==> Stopping old container %s (if any)...", containerName)
	p.stopContainer(ctx, containerName, logFn)

	// 6. Parse env vars
	envMap, err := app.EnvMap()
	if err != nil {
		logFn("warning: could not parse env vars: %v", err)
		envMap = make(map[string]string)
	}
	envList := make([]string, 0, len(envMap))
	for k, v := range envMap {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	// 7. Create and start new container
	logFn("==> Starting container %s...", containerName)
	cPort := nat.Port(fmt.Sprintf("%d/tcp", app.ContainerPort))
	// Bind to 127.0.0.1 when Caddy is on the same host (localhost).
	// Bind to all interfaces when Caddy reaches the host remotely (e.g. host.docker.internal).
	bindIP := "127.0.0.1"
	if p.upstreamHost != "localhost" {
		bindIP = "0.0.0.0"
	}
	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			cPort: []nat.PortBinding{{
				HostIP:   bindIP,
				HostPort: fmt.Sprintf("%d", app.HostPort),
			}},
		},
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
	}
	containerConfig := &container.Config{
		Image: imageTag,
		Env:   envList,
		ExposedPorts: nat.PortSet{
			cPort: struct{}{},
		},
	}

	createResp, err := p.docker.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}

	if err := p.docker.ContainerStart(ctx, createResp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	logFn("==> Container started: %s", createResp.ID[:12])

	// 8. Save container ID + image ID
	_ = p.db.SetDeploymentContainer(depID, imageTag, createResp.ID)

	// 9. Mark old deployment as stopped
	if prevDep != nil && prevDep.Status == store.StatusRunning {
		logFn("==> Marking previous deployment %s as stopped", prevDep.ID)
		_ = p.db.UpdateDeploymentStatus(prevDep.ID, store.StatusStopped, "superseded by new deployment")
	}

	// Set status = running
	_ = p.db.UpdateDeploymentStatus(depID, store.StatusRunning, "")
	logFn("==> Deployment complete!")

	// 10. Sync Caddy
	if err := p.syncCaddy(ctx); err != nil {
		logFn("warning: caddy sync failed: %v", err)
	}

	// 11. Cleanup old images for this app
	go p.cleanupOldImages(context.Background(), app.Name)

	return nil
}

// projectConfig holds optional per-project settings from at.json.
type projectConfig struct {
	Domain        string            `json:"domain"`
	ContainerPort int               `json:"container_port"`
	EnvVars       map[string]string `json:"env_vars"`
}

func readProjectConfig(dir string) projectConfig {
	var cfg projectConfig
	data, err := os.ReadFile(filepath.Join(dir, "at.json"))
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

// ScanProjects scans the projects directory and registers any new subdirectories as apps.
func (p *Pipeline) ScanProjects() error {
	entries, err := os.ReadDir(p.projectsDir)
	if err != nil {
		return fmt.Errorf("read projects dir %s: %w", p.projectsDir, err)
	}

	registered := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())

		if !validAppName.MatchString(name) {
			log.Printf("[scan] skipping %q: name is not valid for Docker container names", entry.Name())
			continue
		}

		existing, err := p.db.GetAppByName(name)
		if err != nil {
			log.Printf("[scan] error checking app %s: %v", name, err)
			continue
		}
		if existing != nil {
			continue // Already registered
		}

		cfg := readProjectConfig(filepath.Join(p.projectsDir, entry.Name()))

		domain := cfg.Domain
		if domain == "" && p.baseDomain != "" {
			domain = name + "." + p.baseDomain
		}
		if domain == "" {
			domain = name + ".localhost"
		}

		containerPort := cfg.ContainerPort
		if containerPort == 0 {
			containerPort = 8080
		}

		envVars := "{}"
		if len(cfg.EnvVars) > 0 {
			b, _ := json.Marshal(cfg.EnvVars)
			envVars = string(b)
		}

		app := &store.App{
			ID:            uuid.New().String(),
			Name:          name,
			Domain:        domain,
			ContainerPort: containerPort,
			EnvVars:       envVars,
		}

		if err := p.db.CreateApp(app); err != nil {
			log.Printf("[scan] failed to register app %s: %v", name, err)
			continue
		}

		log.Printf("[scan] registered new app: %s (domain: %s, port: %d)", name, domain, containerPort)
		registered++
	}

	if registered > 0 {
		log.Printf("[scan] registered %d new app(s)", registered)
	} else {
		log.Printf("[scan] no new projects found")
	}
	return nil
}

// ReconcileRunning checks containers marked running in DB actually exist in Docker.
// Marks them stopped if not, then syncs Caddy.
func (p *Pipeline) ReconcileRunning(ctx context.Context) error {
	deps, err := p.db.GetRunningDeployments()
	if err != nil {
		return fmt.Errorf("get running deployments: %w", err)
	}

	for _, dep := range deps {
		if dep.ContainerID == "" {
			_ = p.db.UpdateDeploymentStatus(dep.ID, store.StatusStopped, "no container ID recorded")
			continue
		}

		info, err := p.docker.ContainerInspect(ctx, dep.ContainerID)
		if err != nil {
			if strings.Contains(err.Error(), "No such container") {
				log.Printf("[reconcile] container %s not found, marking deployment %s stopped", shortID(dep.ContainerID), dep.ID)
				_ = p.db.UpdateDeploymentStatus(dep.ID, store.StatusStopped, "container not found on reconcile")
			} else {
				log.Printf("[reconcile] inspect error for %s: %v", dep.ContainerID, err)
			}
			continue
		}

		if !info.State.Running {
			log.Printf("[reconcile] container %s not running, marking deployment %s stopped", shortID(dep.ContainerID), dep.ID)
			_ = p.db.UpdateDeploymentStatus(dep.ID, store.StatusStopped, "container not running on reconcile")
		}
	}

	if err := p.syncCaddy(ctx); err != nil {
		log.Printf("[reconcile] caddy sync warning: %v", err)
	}

	return nil
}

// syncCaddy builds routes from all apps with running deployments and syncs Caddy.
func (p *Pipeline) syncCaddy(ctx context.Context) error {
	apps, err := p.db.ListApps()
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}

	appIDs := make([]string, len(apps))
	for i, app := range apps {
		appIDs[i] = app.ID
	}
	latestDeps, err := p.db.GetLatestDeploymentsByAppIDs(appIDs)
	if err != nil {
		return fmt.Errorf("get latest deployments: %w", err)
	}

	routes := make([]proxy.Route, 0)
	for _, app := range apps {
		dep := latestDeps[app.ID]
		if dep != nil && dep.Status == store.StatusRunning {
			routes = append(routes, proxy.Route{
				Domain:   app.Domain,
				Upstream: fmt.Sprintf("%s:%d", p.upstreamHost, app.HostPort),
			})
		}
	}

	return p.caddy.Sync(ctx, routes)
}

// stopContainer stops and removes a container by name, ignoring "not found" errors.
func (p *Pipeline) stopContainer(ctx context.Context, name string, logFn func(string, ...any)) {
	timeout := 10
	err := p.docker.ContainerStop(ctx, name, container.StopOptions{Timeout: &timeout})
	if err != nil {
		if !strings.Contains(err.Error(), "No such container") {
			logFn("warning: stop container %s: %v", name, err)
		}
	}
	err = p.docker.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
	if err != nil {
		if !strings.Contains(err.Error(), "No such container") {
			logFn("warning: remove container %s: %v", name, err)
		}
	}
}

// cleanupOldImages removes old images for the app, keeping the 2 most recent.
func (p *Pipeline) cleanupOldImages(ctx context.Context, appName string) {
	f := filters.NewArgs()
	f.Add("label", fmt.Sprintf("at.app=%s", appName))

	images, err := p.docker.ImageList(ctx, image.ListOptions{Filters: f})
	if err != nil {
		log.Printf("[cleanup] list images for %s: %v", appName, err)
		return
	}

	// Sort by created time desc (newest first)
	sort.Slice(images, func(i, j int) bool {
		return images[i].Created > images[j].Created
	})

	// Keep the 2 most recent
	for i, img := range images {
		if i < 2 {
			continue
		}
		_, err := p.docker.ImageRemove(ctx, img.ID, image.RemoveOptions{Force: false, PruneChildren: true})
		if err != nil {
			log.Printf("[cleanup] remove image %s: %v", img.ID[:12], err)
		} else {
			log.Printf("[cleanup] removed old image %s for app %s", img.ID[:12], appName)
		}
	}
}

func (p *Pipeline) StopAppContainer(ctx context.Context, app *store.App) {
	containerName := fmt.Sprintf("at-%s", app.Name)
	logFn := func(format string, args ...any) {
		log.Printf("[stop:%s] "+format, append([]any{app.Name}, args...)...)
	}
	p.stopContainer(ctx, containerName, logFn)
}

// SyncCaddyPublic is an exported wrapper for syncCaddy.
func (p *Pipeline) SyncCaddyPublic(ctx context.Context) error {
	return p.syncCaddy(ctx)
}

// shortID returns the first n characters of id, or the whole string if shorter.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// tryLock attempts to acquire a per-app lock. Returns (unlock func, ok).
func (p *Pipeline) tryLock(appID string) (unlock func(), ok bool) {
	ch, _ := p.locks.LoadOrStore(appID, make(chan struct{}, 1))
	sem := ch.(chan struct{})
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
		return nil, false
	}
}

// runCmdInDir runs a command in dir, streaming output in real-time through logFn.
func runCmdInDir(dir string, logFn func(string, ...any), name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for scanner.Scan() {
			logFn("%s", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			logFn("warning: log scanner error: %s", err)
		}
	}()

	err := cmd.Wait()
	pw.Close()
	<-done
	pr.Close()
	return err
}
