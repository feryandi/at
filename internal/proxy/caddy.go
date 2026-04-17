package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const syncRetries = 3
const syncRetryDelay = 2 * time.Second

// Route represents a single reverse proxy route.
type Route struct {
	Domain   string
	Upstream string // "localhost:PORT"
}

// Caddy manages Caddy reverse proxy configuration via admin API.
type Caddy struct {
	adminURL string
	client   *http.Client
}

// NewCaddy creates a new Caddy proxy manager.
func NewCaddy(adminURL string) *Caddy {
	return &Caddy{
		adminURL: adminURL,
		// DisableKeepAlives ensures a fresh TCP connection per request,
		// avoiding EOF errors from stale keep-alive connections after Caddy restarts.
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		},
	}
}

// Ping returns true if the Caddy admin API is reachable.
func (c *Caddy) Ping(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.adminURL+"/config/", nil)
	if err != nil {
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

type caddyConfig struct {
	Admin caddyAdmin `json:"admin"`
	Apps  struct {
		HTTP caddyHTTP `json:"http"`
	} `json:"apps"`
}

type caddyAdmin struct {
	Listen string `json:"listen"`
}

type caddyHTTP struct {
	Servers map[string]caddyServer `json:"servers"`
}

type caddyServer struct {
	Listen         []string          `json:"listen"`
	AutomaticHTTPS *caddyAutoHTTPS   `json:"automatic_https,omitempty"`
	Routes         []caddyRoute      `json:"routes"`
}

type caddyAutoHTTPS struct {
	Skip []string `json:"skip,omitempty"` // domains that should not get HTTPS
}

// adminListenAddr derives the Caddy admin listen address from the admin URL.
// It always binds to 0.0.0.0 so the admin API remains reachable when Caddy
// runs inside Docker (where 127.0.0.1 is the container loopback, not the host).
func adminListenAddr(adminURL string) string {
	u, err := url.Parse(adminURL)
	if err != nil {
		return "0.0.0.0:2019"
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return "0.0.0.0:2019"
	}
	return "0.0.0.0:" + port
}

// isLocalDomain returns true for domains that can't get a Let's Encrypt cert:
// localhost, *.localhost, *.local, bare IPs, and 127.x.x.x.
func isLocalDomain(domain string) bool {
	if domain == "localhost" {
		return true
	}
	if strings.HasSuffix(domain, ".localhost") || strings.HasSuffix(domain, ".local") {
		return true
	}
	if ip := net.ParseIP(domain); ip != nil {
		return true
	}
	return false
}

type caddyRoute struct {
	Match  []map[string]any `json:"match"`
	Handle []map[string]any `json:"handle"`
}

// Sync replaces the full Caddy config with the given routes.
// Retries on transient errors (EOF, connection reset) to handle Caddy startup timing.
func (c *Caddy) Sync(ctx context.Context, routes []Route) error {
	var lastErr error
	for attempt := 1; attempt <= syncRetries; attempt++ {
		lastErr = c.syncOnce(ctx, routes)
		if lastErr == nil {
			return nil
		}
		if attempt < syncRetries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(syncRetryDelay):
			}
		}
	}
	return lastErr
}

func (c *Caddy) syncOnce(ctx context.Context, routes []Route) error {
	caddyRoutes := make([]caddyRoute, 0, len(routes))
	for _, r := range routes {
		caddyRoutes = append(caddyRoutes, caddyRoute{
			Match: []map[string]any{
				{"host": []string{r.Domain}},
			},
			Handle: []map[string]any{
				{
					"handler":   "reverse_proxy",
					"upstreams": []map[string]any{{"dial": r.Upstream}},
				},
			},
		})
	}

	// Collect local domains that can't get Let's Encrypt certificates.
	var skipHTTPS []string
	for _, r := range routes {
		if isLocalDomain(r.Domain) {
			skipHTTPS = append(skipHTTPS, r.Domain)
		}
	}

	server := caddyServer{
		Listen: []string{":80", ":443"},
		Routes: caddyRoutes,
	}
	if len(skipHTTPS) > 0 {
		server.AutomaticHTTPS = &caddyAutoHTTPS{Skip: skipHTTPS}
	}

	var cfg caddyConfig
	// Always include the admin section so POST /load doesn't reset the admin
	// listener to its default (container-local 127.0.0.1:2019), which would
	// break Docker's port forwarding and make Caddy unreachable after the first sync.
	cfg.Admin.Listen = adminListenAddr(c.adminURL)
	cfg.Apps.HTTP.Servers = map[string]caddyServer{"main": server}

	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal caddy config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.adminURL+"/load", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build caddy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("caddy unreachable (is Caddy running?): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("caddy returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
