package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	syncRetries   = 3
	syncRetryDelay = 2 * time.Second
	atRoutePrefix  = "at-" // @id prefix for routes managed by at
)

// Route represents a single reverse proxy route.
type Route struct {
	Domain   string
	Upstream string // "localhost:PORT"
}

// Caddy manages Caddy reverse proxy configuration via admin API.
type Caddy struct {
	adminURL    string
	oauthPolicy string // caddy-security authorization policy name; empty = no auth on app routes
	client      *http.Client
}

// NewCaddy creates a new Caddy proxy manager.
// oauthPolicy is the caddy-security authorization policy name to enforce on all app routes (empty = disabled).
func NewCaddy(adminURL, oauthPolicy string) *Caddy {
	return &Caddy{
		adminURL:    adminURL,
		oauthPolicy: oauthPolicy,
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

// Sync reads the current Caddy config, replaces at-managed routes, and reloads.
// Caddyfile-defined servers are preserved. Retries on transient errors.
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
	cfg, err := c.fetchConfig(ctx)
	if err != nil {
		return fmt.Errorf("fetch caddy config: %w", err)
	}
	mergeAtRoutes(cfg, routes, c.oauthPolicy)
	return c.postLoad(ctx, cfg)
}

func (c *Caddy) fetchConfig(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.adminURL+"/config/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("caddy unreachable: %w", err)
	}
	defer resp.Body.Close()
	var cfg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

func (c *Caddy) postLoad(ctx context.Context, cfg map[string]any) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
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

// mergeAtRoutes removes all previously at-managed routes (identified by "@id" prefix "at-")
// from every server in cfg, then inserts the new routes into the :443 server.
// If no :443 server exists, a dedicated "at" server is created.
func mergeAtRoutes(cfg map[string]any, routes []Route, oauthPolicy string) {
	servers := ensureServersMap(cfg)

	// Collect at-managed local domains currently in skip lists so we can clean them up.
	prevLocalDomains := collectAtLocalDomains(servers)

	// Strip all at-managed routes from every server.
	for name, srv := range servers {
		s, _ := srv.(map[string]any)
		if s == nil {
			continue
		}
		s["routes"] = withoutAtRoutes(s["routes"])
		servers[name] = s
	}

	// Find the server that handles :443.
	target := findServerByListen(servers, ":443")

	if len(routes) == 0 {
		// Nothing to add — remove the local-domain HTTPS skip entries we previously added.
		if target != "" {
			if s, ok := servers[target].(map[string]any); ok {
				setSkipList(s, removeStrings(getSkipList(s), prevLocalDomains))
				servers[target] = s
			}
		}
		return
	}

	if target == "" {
		// No existing :443 server — create a dedicated one.
		// Only add :80 if nothing else claims it, to avoid listener conflicts.
		listen := []any{":443"}
		if findServerByListen(servers, ":80") == "" {
			listen = append([]any{":80"}, listen...)
		}
		target = "at"
		servers["at"] = map[string]any{"listen": listen, "routes": []any{}}
	}

	s := servers[target].(map[string]any)

	// Prepend at-managed routes so they take precedence over Caddyfile catch-alls.
	existing, _ := s["routes"].([]any)
	newRoutes := buildAtRoutesWithPolicy(routes, oauthPolicy)
	merged := make([]any, 0, len(newRoutes)+len(existing))
	merged = append(merged, newRoutes...)
	merged = append(merged, existing...)
	s["routes"] = merged

	// Update automatic_https.skip: remove previous at-local entries, add current ones.
	var newLocal []string
	for _, r := range routes {
		if isLocalDomain(r.Domain) {
			newLocal = append(newLocal, r.Domain)
		}
	}
	updated := removeStrings(getSkipList(s), prevLocalDomains)
	updated = append(updated, newLocal...)
	setSkipList(s, updated)

	servers[target] = s
}

// ensureServersMap navigates cfg→apps→http→servers, creating intermediate maps as needed.
func ensureServersMap(cfg map[string]any) map[string]any {
	apps, _ := cfg["apps"].(map[string]any)
	if apps == nil {
		apps = map[string]any{}
		cfg["apps"] = apps
	}
	httpApp, _ := apps["http"].(map[string]any)
	if httpApp == nil {
		httpApp = map[string]any{}
		apps["http"] = httpApp
	}
	servers, _ := httpApp["servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		httpApp["servers"] = servers
	}
	return servers
}

// findServerByListen returns the first server name whose listen list contains addr.
func findServerByListen(servers map[string]any, addr string) string {
	for name, srv := range servers {
		s, _ := srv.(map[string]any)
		if s == nil {
			continue
		}
		for _, l := range toStringSlice(s["listen"]) {
			if l == addr || strings.HasSuffix(l, addr) {
				return name
			}
		}
	}
	return ""
}

// withoutAtRoutes removes routes whose "@id" starts with atRoutePrefix.
func withoutAtRoutes(raw any) []any {
	routes, _ := raw.([]any)
	filtered := make([]any, 0, len(routes))
	for _, r := range routes {
		route, _ := r.(map[string]any)
		if route == nil {
			filtered = append(filtered, r)
			continue
		}
		if id, _ := route["@id"].(string); strings.HasPrefix(id, atRoutePrefix) {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

// collectAtLocalDomains returns the local domains currently tracked in at-managed routes.
func collectAtLocalDomains(servers map[string]any) []string {
	seen := map[string]bool{}
	for _, srv := range servers {
		s, _ := srv.(map[string]any)
		if s == nil {
			continue
		}
		routes, _ := s["routes"].([]any)
		for _, r := range routes {
			route, _ := r.(map[string]any)
			if route == nil {
				continue
			}
			id, _ := route["@id"].(string)
			if !strings.HasPrefix(id, atRoutePrefix) {
				continue
			}
			if d := routeDomain(route); d != "" && isLocalDomain(d) {
				seen[d] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	return out
}

func routeDomain(route map[string]any) string {
	matches, _ := route["match"].([]any)
	if len(matches) == 0 {
		return ""
	}
	m, _ := matches[0].(map[string]any)
	if m == nil {
		return ""
	}
	hosts := toStringSlice(m["host"])
	if len(hosts) == 0 {
		return ""
	}
	return hosts[0]
}

func getSkipList(server map[string]any) []string {
	ah, _ := server["automatic_https"].(map[string]any)
	if ah == nil {
		return nil
	}
	return toStringSlice(ah["skip"])
}

func setSkipList(server map[string]any, domains []string) {
	ah, _ := server["automatic_https"].(map[string]any)
	if len(domains) == 0 {
		if ah != nil {
			delete(ah, "skip")
			if len(ah) == 0 {
				delete(server, "automatic_https")
			}
		}
		return
	}
	if ah == nil {
		ah = map[string]any{}
		server["automatic_https"] = ah
	}
	skipAny := make([]any, len(domains))
	for i, d := range domains {
		skipAny[i] = d
	}
	ah["skip"] = skipAny
}

func removeStrings(list, remove []string) []string {
	rm := make(map[string]bool, len(remove))
	for _, s := range remove {
		rm[s] = true
	}
	out := make([]string, 0, len(list))
	for _, s := range list {
		if !rm[s] {
			out = append(out, s)
		}
	}
	return out
}

func buildAtRoutes(routes []Route) []any {
	return buildAtRoutesWithPolicy(routes, "")
}

func buildAtRoutesWithPolicy(routes []Route, oauthPolicy string) []any {
	result := make([]any, 0, len(routes))
	for _, r := range routes {
		handle := []any{}
		if oauthPolicy != "" {
			handle = append(handle, map[string]any{
				"handler": "authentication",
				"providers": map[string]any{
					"authorizer": map[string]any{
						"gatekeeper_name": oauthPolicy,
						"route_matcher":   "*",
					},
				},
			})
		}
		handle = append(handle, map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": []any{map[string]any{"dial": r.Upstream}},
		})
		result = append(result, map[string]any{
			"@id": atRoutePrefix + r.Domain,
			"match": []any{
				map[string]any{"host": []any{r.Domain}},
			},
			"handle": handle,
		})
	}
	return result
}

func toStringSlice(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, a := range arr {
		if s, _ := a.(string); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isLocalDomain returns true for domains that can't get a Let's Encrypt cert:
// localhost, *.localhost, *.local, bare IPs.
func isLocalDomain(domain string) bool {
	if domain == "localhost" {
		return true
	}
	if strings.HasSuffix(domain, ".localhost") || strings.HasSuffix(domain, ".local") {
		return true
	}
	return net.ParseIP(domain) != nil
}
