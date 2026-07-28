package main

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Digest auth compatibility only; Shelly normally uses SHA-256.
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls" //nolint:gosec // Optional self-signed device certificate support is user-controlled.
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxRPCResponseBytes = 4 << 20 // 4 MiB

//go:embed index.html
var indexHTML string

type Config struct {
	Listen                string         `json:"listen"`
	RefreshSeconds        int            `json:"refresh_seconds"`
	RequestTimeoutSeconds int            `json:"request_timeout_seconds"`
	UIUsername            string         `json:"ui_username"`
	UIPassword            string         `json:"ui_password"`
	Devices               []DeviceConfig `json:"devices"`
	GPIO                  GPIOConfig     `json:"gpio"`
}

// GPIOConfig controls the physical-button side of the controller. Every field
// is optional; the zero value watches the whole 40-pin header with pull-ups.
type GPIOConfig struct {
	Enabled      *bool  `json:"enabled,omitempty"`
	Chip         string `json:"chip,omitempty"`
	Consumer     string `json:"consumer,omitempty"`
	DefaultBias  string `json:"default_bias,omitempty"`
	DebounceMS   int    `json:"debounce_ms,omitempty"`
	BindingsPath string `json:"bindings_path,omitempty"`
	Pins         []int  `json:"pins,omitempty"`
}

type DeviceConfig struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	URL                string `json:"url"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
}

type ShellyClient struct {
	cfg        DeviceConfig
	baseURL    string
	httpClient *http.Client
}

type App struct {
	cfg         Config
	clients     map[string]*ShellyClient
	gpio        *GPIOWatcher
	buttons     *ButtonEngine
	pinDefaults PinConfig
	gpioReason  string
}

type DeviceSnapshot struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Online      bool              `json:"online"`
	RetrievedAt time.Time         `json:"retrieved_at"`
	LatencyMS   int64             `json:"latency_ms"`
	DeviceInfo  json.RawMessage   `json:"device_info,omitempty"`
	Status      json.RawMessage   `json:"status,omitempty"`
	Config      json.RawMessage   `json:"config,omitempty"`
	Methods     json.RawMessage   `json:"methods,omitempty"`
	Components  json.RawMessage   `json:"components,omitempty"`
	Errors      map[string]string `json:"errors,omitempty"`
}

type componentsPage struct {
	Components []json.RawMessage `json:"components"`
	CfgRev     int64             `json:"cfg_rev"`
	Offset     int               `json:"offset"`
	Total      int               `json:"total"`
}

type componentsResult struct {
	Components []json.RawMessage `json:"components"`
	CfgRev     int64             `json:"cfg_rev"`
	Offset     int               `json:"offset"`
	Total      int               `json:"total"`
}

func main() {
	configPath := flag.String("config", "config.json", "path to JSON configuration file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	app, err := newApp(cfg)
	if err != nil {
		log.Fatalf("startup error: %v", err)
	}

	defer app.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.withUIAuth(app.handleIndex))
	mux.HandleFunc("/healthz", app.handleHealth)
	mux.HandleFunc("/api/devices", app.withUIAuth(app.handleDevices))
	mux.HandleFunc("/api/devices/", app.withUIAuth(app.handleDevice))
	mux.HandleFunc("/gpio", app.withUIAuth(app.handleGPIOPage))
	mux.HandleFunc("/api/gpio", app.withUIAuth(app.handleGPIOState))
	mux.HandleFunc("/api/gpio/stream", app.withUIAuth(app.handleGPIOStream))
	mux.HandleFunc("/api/gpio/config", app.withUIAuth(app.handleGPIOConfig))
	mux.HandleFunc("/api/gpio/reset", app.withUIAuth(app.handleGPIOReset))
	mux.HandleFunc("/api/bindings", app.withUIAuth(app.handleBindings))

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	if !isLoopbackListen(cfg.Listen) && cfg.UIPassword == "" {
		log.Printf("WARNING: listening on %s without UI authentication; anyone who can reach this server can control the relays", cfg.Listen)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Shelly controller listening on http://%s", cfg.Listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}

func loadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}

	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8080"
	}
	if cfg.RefreshSeconds <= 0 {
		cfg.RefreshSeconds = 5
	}
	if cfg.RequestTimeoutSeconds <= 0 {
		cfg.RequestTimeoutSeconds = 5
	}
	cfg.UIPassword = expandEnvReference(cfg.UIPassword)
	if cfg.UIPassword != "" && cfg.UIUsername == "" {
		cfg.UIUsername = "shelly"
	}
	if len(cfg.Devices) == 0 {
		return Config{}, errors.New("at least one device must be configured")
	}

	seen := make(map[string]struct{}, len(cfg.Devices))
	for i := range cfg.Devices {
		d := &cfg.Devices[i]
		d.ID = strings.TrimSpace(d.ID)
		d.Name = strings.TrimSpace(d.Name)
		d.URL = strings.TrimSpace(d.URL)
		d.Username = strings.TrimSpace(d.Username)
		d.Password = expandEnvReference(d.Password)
		if d.ID == "" {
			return Config{}, fmt.Errorf("devices[%d].id is required", i)
		}
		if strings.ContainsAny(d.ID, "/\\") {
			return Config{}, fmt.Errorf("devices[%d].id must not contain slashes", i)
		}
		if _, ok := seen[d.ID]; ok {
			return Config{}, fmt.Errorf("duplicate device id %q", d.ID)
		}
		seen[d.ID] = struct{}{}
		if d.Name == "" {
			d.Name = d.ID
		}
		if d.URL == "" {
			return Config{}, fmt.Errorf("devices[%d].url is required", i)
		}
		if !strings.Contains(d.URL, "://") {
			d.URL = "http://" + d.URL
		}
		if d.Password != "" && d.Username == "" {
			d.Username = "admin"
		}
	}

	if err := normalizeGPIOConfig(&cfg.GPIO, filepath.Dir(path)); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func normalizeGPIOConfig(g *GPIOConfig, configDir string) error {
	if g.Enabled == nil {
		enabled := true
		g.Enabled = &enabled
	}
	if g.Chip == "" {
		g.Chip = "gpiochip0"
	}
	if g.Consumer == "" {
		g.Consumer = "goshelly"
	}
	switch g.DefaultBias {
	case "":
		g.DefaultBias = biasPullUp
	case biasPullUp, biasPullDown, biasNone:
	default:
		return fmt.Errorf("gpio.default_bias must be %q, %q or %q", biasPullUp, biasPullDown, biasNone)
	}
	if g.DebounceMS == 0 {
		g.DebounceMS = 20
	}
	if g.DebounceMS < 0 || g.DebounceMS > 2000 {
		return errors.New("gpio.debounce_ms must be between 0 and 2000")
	}
	if g.BindingsPath == "" {
		g.BindingsPath = filepath.Join(configDir, "bindings.json")
	}
	if len(g.Pins) == 0 {
		g.Pins = headerGPIOs()
	}
	for _, pin := range g.Pins {
		if pin < 0 || pin > 63 {
			return fmt.Errorf("gpio.pins contains out-of-range line %d", pin)
		}
	}
	return nil
}

func expandEnvReference(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 4 && strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		name := value[2 : len(value)-1]
		if name != "" && !strings.ContainsAny(name, "${}") {
			return os.Getenv(name)
		}
	}
	return value
}

func newApp(cfg Config) (*App, error) {
	clients := make(map[string]*ShellyClient, len(cfg.Devices))
	for _, d := range cfg.Devices {
		client, err := newShellyClient(d, time.Duration(cfg.RequestTimeoutSeconds)*time.Second)
		if err != nil {
			return nil, fmt.Errorf("device %q: %w", d.ID, err)
		}
		clients[d.ID] = client
	}

	app := &App{
		cfg:     cfg,
		clients: clients,
		pinDefaults: PinConfig{
			Bias:     cfg.GPIO.DefaultBias,
			Debounce: time.Duration(cfg.GPIO.DebounceMS) * time.Millisecond,
		},
	}
	if err := app.startGPIO(); err != nil {
		return nil, err
	}
	return app, nil
}

// startGPIO brings up the watcher and button engine. A machine without a GPIO
// chip is not an error: the relay UI still works and the visualizer explains
// why it is empty, which keeps the same binary usable on a dev laptop.
func (a *App) startGPIO() error {
	if !*a.cfg.GPIO.Enabled {
		a.gpioReason = "disabled in configuration"
		return nil
	}

	overrides := make(map[int]PinConfig)
	if bindings, err := loadBindings(a.cfg.GPIO.BindingsPath); err == nil {
		for _, b := range bindings {
			cfg := a.pinDefaults
			if b.Bias != "" {
				cfg.Bias = b.Bias
			}
			if b.DebounceMS > 0 {
				cfg.Debounce = time.Duration(b.DebounceMS) * time.Millisecond
			}
			overrides[b.GPIO] = cfg
		}
	}

	watcher, err := NewGPIOWatcher(a.cfg.GPIO.Chip, a.cfg.GPIO.Consumer, a.cfg.GPIO.Pins, a.pinDefaults, overrides)
	if err != nil {
		a.gpioReason = err.Error()
		log.Printf("WARNING: GPIO unavailable, physical buttons are off: %v", err)
		return nil
	}
	a.gpio = watcher

	engine, err := NewButtonEngine(a, watcher, a.cfg.GPIO.BindingsPath,
		a.pinDefaults, time.Duration(a.cfg.RequestTimeoutSeconds)*time.Second)
	if err != nil {
		_ = watcher.Close()
		a.gpio = nil
		return fmt.Errorf("button engine: %w", err)
	}
	a.buttons = engine

	log.Printf("GPIO watching %d lines on %s (bias %s, debounce %dms), %d binding(s) from %s",
		len(a.cfg.GPIO.Pins), a.cfg.GPIO.Chip, a.cfg.GPIO.DefaultBias, a.cfg.GPIO.DebounceMS,
		len(engine.Bindings()), a.cfg.GPIO.BindingsPath)
	return nil
}

// Close stops the button engine before releasing the GPIO lines.
func (a *App) Close() {
	if a.buttons != nil {
		a.buttons.Close()
	}
	if a.gpio != nil {
		_ = a.gpio.Close()
	}
}

func newShellyClient(cfg DeviceConfig, timeout time.Duration) (*ShellyClient, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL scheme must be http or https")
	}
	if u.Host == "" {
		return nil, errors.New("URL must include a host")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("URL must not include a path")
	}
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""

	transport := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{ //nolint:gosec
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		},
	}

	return &ShellyClient{
		cfg:     cfg,
		baseURL: strings.TrimRight(u.String(), "/"),
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}, nil
}

func (c *ShellyClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "" || strings.Contains(method, "/") {
		return nil, errors.New("invalid RPC method")
	}
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal RPC parameters: %w", err)
	}

	endpoint := c.baseURL + "/rpc/" + url.PathEscape(method)
	resp, err := c.doRPCRequest(ctx, endpoint, body, "")
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := digestChallenge(resp.Header.Values("WWW-Authenticate"))
		_ = resp.Body.Close()
		if challenge == "" {
			return nil, errors.New("device requires authentication but did not provide a Digest challenge")
		}
		if c.cfg.Password == "" {
			return nil, errors.New("device requires authentication; configure its password")
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		auth, err := buildDigestAuthorization(req, challenge, c.cfg.Username, c.cfg.Password)
		if err != nil {
			return nil, fmt.Errorf("build Digest authentication: %w", err)
		}
		resp, err = c.doRPCRequest(ctx, endpoint, body, auth)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read device response: %w", err)
	}
	if len(payload) > maxRPCResponseBytes {
		return nil, fmt.Errorf("device response exceeds %d bytes", maxRPCResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("device returned HTTP %d: %s", resp.StatusCode, compactBody(payload))
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return json.RawMessage("null"), nil
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("device returned invalid JSON: %s", compactBody(payload))
	}

	var rpcErr struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &rpcErr) == nil && rpcErr.Code != nil && rpcErr.Message != "" {
		return nil, fmt.Errorf("Shelly RPC error %d: %s", *rpcErr.Code, rpcErr.Message)
	}

	return json.RawMessage(payload), nil
}

func (c *ShellyClient) doRPCRequest(ctx context.Context, endpoint string, body []byte, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Connection", "close")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", req.URL.Host, err)
	}
	return resp, nil
}

func digestChallenge(headers []string) string {
	for _, h := range headers {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(h)), "digest ") {
			return strings.TrimSpace(h)
		}
	}
	return ""
}

func buildDigestAuthorization(req *http.Request, challenge, username, password string) (string, error) {
	challenge = strings.TrimSpace(challenge)
	if !strings.HasPrefix(strings.ToLower(challenge), "digest ") {
		return "", errors.New("unsupported authentication challenge")
	}
	params := parseAuthParams(strings.TrimSpace(challenge[len("Digest "):]))
	realm := params["realm"]
	nonce := params["nonce"]
	if realm == "" || nonce == "" {
		return "", errors.New("Digest challenge is missing realm or nonce")
	}
	if username == "" {
		username = "admin"
	}

	algorithm := params["algorithm"]
	if algorithm == "" {
		algorithm = "MD5"
	}
	algorithmUpper := strings.ToUpper(algorithm)
	baseAlgorithm := strings.TrimSuffix(algorithmUpper, "-SESS")
	isSession := strings.HasSuffix(algorithmUpper, "-SESS")
	if baseAlgorithm != "SHA-256" && baseAlgorithm != "MD5" {
		return "", fmt.Errorf("unsupported Digest algorithm %q", algorithm)
	}

	cnonceBytes := make([]byte, 16)
	if _, err := rand.Read(cnonceBytes); err != nil {
		return "", fmt.Errorf("generate cnonce: %w", err)
	}
	cnonce := hex.EncodeToString(cnonceBytes)
	nc := "00000001"
	uri := req.URL.RequestURI()

	ha1 := digestHash(baseAlgorithm, username+":"+realm+":"+password)
	if isSession {
		ha1 = digestHash(baseAlgorithm, ha1+":"+nonce+":"+cnonce)
	}
	ha2 := digestHash(baseAlgorithm, req.Method+":"+uri)

	qop := chooseQOP(params["qop"])
	var response string
	if qop != "" {
		if qop != "auth" {
			return "", fmt.Errorf("unsupported Digest qop %q", qop)
		}
		response = digestHash(baseAlgorithm, ha1+":"+nonce+":"+nc+":"+cnonce+":"+qop+":"+ha2)
	} else {
		response = digestHash(baseAlgorithm, ha1+":"+nonce+":"+ha2)
	}

	parts := []string{
		`username="` + quoteDigest(username) + `"`,
		`realm="` + quoteDigest(realm) + `"`,
		`nonce="` + quoteDigest(nonce) + `"`,
		`uri="` + quoteDigest(uri) + `"`,
		`algorithm=` + algorithm,
		`response="` + response + `"`,
	}
	if opaque := params["opaque"]; opaque != "" {
		parts = append(parts, `opaque="`+quoteDigest(opaque)+`"`)
	}
	if qop != "" {
		parts = append(parts, "qop="+qop, "nc="+nc, `cnonce="`+cnonce+`"`)
	}
	return "Digest " + strings.Join(parts, ", "), nil
}

func parseAuthParams(s string) map[string]string {
	result := make(map[string]string)
	for _, token := range splitHeaderCSV(s) {
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			} else {
				value = value[1 : len(value)-1]
			}
		}
		result[key] = value
	}
	return result
}

func splitHeaderCSV(s string) []string {
	var parts []string
	start := 0
	quoted := false
	escaped := false
	for i, r := range s {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == ',' && !quoted {
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts
}

func chooseQOP(qops string) string {
	if strings.TrimSpace(qops) == "" {
		return ""
	}
	for _, q := range strings.Split(qops, ",") {
		if strings.EqualFold(strings.TrimSpace(q), "auth") {
			return "auth"
		}
	}
	return strings.ToLower(strings.TrimSpace(strings.Split(qops, ",")[0]))
}

func digestHash(algorithm, value string) string {
	switch algorithm {
	case "SHA-256":
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:])
	case "MD5":
		sum := md5.Sum([]byte(value)) //nolint:gosec // Required only for RFC 7616 fallback compatibility.
		return hex.EncodeToString(sum[:])
	default:
		panic("unsupported digest hash")
	}
}

func quoteDigest(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func compactBody(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

func (a *App) overview(ctx context.Context, client *ShellyClient) DeviceSnapshot {
	result := DeviceSnapshot{
		ID:          client.cfg.ID,
		Name:        client.cfg.Name,
		URL:         client.baseURL,
		RetrievedAt: time.Now().UTC(),
		Errors:      make(map[string]string),
	}
	started := time.Now()

	if raw, err := client.Call(ctx, "Shelly.GetDeviceInfo", nil); err != nil {
		result.Errors["device_info"] = err.Error()
	} else {
		result.DeviceInfo = raw
	}
	if raw, err := client.Call(ctx, "Shelly.GetStatus", nil); err != nil {
		result.Errors["status"] = err.Error()
	} else {
		result.Status = raw
	}

	result.Online = len(result.DeviceInfo) > 0 || len(result.Status) > 0
	result.LatencyMS = time.Since(started).Milliseconds()
	if len(result.Errors) == 0 {
		result.Errors = nil
	}
	return result
}

func (a *App) fullSnapshot(ctx context.Context, client *ShellyClient) DeviceSnapshot {
	result := a.overview(ctx, client)
	if result.Errors == nil {
		result.Errors = make(map[string]string)
	}
	started := time.Now()

	if raw, err := client.Call(ctx, "Shelly.GetConfig", nil); err != nil {
		result.Errors["config"] = err.Error()
	} else {
		result.Config = raw
	}
	if raw, err := client.Call(ctx, "Shelly.ListMethods", nil); err != nil {
		result.Errors["methods"] = err.Error()
	} else {
		result.Methods = raw
	}
	if raw, err := getAllComponents(ctx, client); err != nil {
		result.Errors["components"] = err.Error()
	} else {
		result.Components = raw
	}

	result.LatencyMS += time.Since(started).Milliseconds()
	if len(result.Errors) == 0 {
		result.Errors = nil
	}
	return result
}

func getAllComponents(ctx context.Context, client *ShellyClient) (json.RawMessage, error) {
	all := componentsResult{Components: make([]json.RawMessage, 0), Offset: 0}
	offset := 0
	for pageNo := 0; pageNo < 100; pageNo++ {
		raw, err := client.Call(ctx, "Shelly.GetComponents", map[string]any{
			"offset":  offset,
			"include": []string{"config", "status"},
		})
		if err != nil {
			return nil, err
		}
		var page componentsPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode component page: %w", err)
		}
		if pageNo == 0 {
			all.CfgRev = page.CfgRev
			all.Total = page.Total
		}
		all.Components = append(all.Components, page.Components...)
		if len(all.Components) >= page.Total || len(page.Components) == 0 {
			all.Total = page.Total
			encoded, err := json.Marshal(all)
			return json.RawMessage(encoded), err
		}
		offset = page.Offset + len(page.Components)
	}
	return nil, errors.New("component pagination exceeded 100 pages")
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	html := strings.ReplaceAll(indexHTML, "__REFRESH_MS__", strconv.Itoa(a.cfg.RefreshSeconds*1000))
	_, _ = io.WriteString(w, html)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().UTC()})
}

func (a *App) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/devices" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	results := make([]DeviceSnapshot, len(a.cfg.Devices))
	var wg sync.WaitGroup
	for i := range a.cfg.Devices {
		client := a.clients[a.cfg.Devices[i].ID]
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = a.overview(r.Context(), client)
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"devices": results})
}

func (a *App) handleDevice(w http.ResponseWriter, r *http.Request) {
	remainder := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/devices/"), "/")
	if remainder == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(remainder, "/")
	deviceID, err := url.PathUnescape(parts[0])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid device id"})
		return
	}
	client, ok := a.clients[deviceID]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device"})
		return
	}

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, a.fullSnapshot(r.Context(), client))
		return
	}

	if len(parts) != 2 || r.Method != http.MethodPost {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown endpoint"})
		return
	}
	if r.Header.Get("X-Shelly-Control") != "1" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing X-Shelly-Control: 1 header"})
		return
	}

	switch parts[1] {
	case "relay":
		a.handleRelay(w, r, client)
	case "toggle":
		a.handleToggle(w, r, client)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown endpoint"})
	}
}

func (a *App) handleRelay(w http.ResponseWriter, r *http.Request, client *ShellyClient) {
	var input struct {
		On          *bool    `json:"on"`
		SwitchID    *int     `json:"switch_id,omitempty"`
		ToggleAfter *float64 `json:"toggle_after,omitempty"`
	}
	if err := decodeJSONBody(w, r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if input.On == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "field 'on' is required"})
		return
	}
	switchID := 0
	if input.SwitchID != nil {
		switchID = *input.SwitchID
	}
	if switchID < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "switch_id must be non-negative"})
		return
	}
	params := map[string]any{"id": switchID, "on": *input.On, "tag": "go-web"}
	if input.ToggleAfter != nil {
		if *input.ToggleAfter <= 0 || *input.ToggleAfter > 86400 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "toggle_after must be greater than 0 and no more than 86400 seconds"})
			return
		}
		params["toggle_after"] = *input.ToggleAfter
	}

	result, err := client.Call(r.Context(), "Switch.Set", params)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	expected := input.On
	if input.ToggleAfter != nil {
		expected = nil // the device will flip back on its own; don't wait for it
	}
	status, statusErr := settledSwitchStatus(r.Context(), client, switchID, expected)
	response := map[string]any{"ok": true, "rpc_result": result}
	if statusErr != nil {
		response["status_error"] = statusErr.Error()
	} else {
		response["switch_status"] = status
	}
	writeJSON(w, http.StatusOK, response)
}

// settledSwitchStatus reads the switch status after a write, giving the relay a
// moment to actually actuate. Reading immediately races the device and returns
// the pre-write output, which makes the UI show the wrong state.
//
// When the caller knows the value to expect, polling stops as soon as the
// device agrees; otherwise a single short delay is used.
func settledSwitchStatus(ctx context.Context, client *ShellyClient, switchID int, expected *bool) (json.RawMessage, error) {
	const (
		attempts = 6
		interval = 90 * time.Millisecond
	)

	var status json.RawMessage
	var err error
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			if status != nil {
				return status, nil
			}
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		status, err = client.Call(ctx, "Switch.GetStatus", map[string]any{"id": switchID})
		if err != nil {
			return nil, err
		}
		if expected == nil {
			return status, nil
		}

		var parsed struct {
			Output *bool `json:"output"`
		}
		if json.Unmarshal(status, &parsed) == nil && parsed.Output != nil && *parsed.Output == *expected {
			return status, nil
		}
	}
	return status, nil
}

func (a *App) handleToggle(w http.ResponseWriter, r *http.Request, client *ShellyClient) {
	var input struct {
		SwitchID *int `json:"switch_id,omitempty"`
	}
	if err := decodeJSONBody(w, r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	switchID := 0
	if input.SwitchID != nil {
		switchID = *input.SwitchID
	}
	if switchID < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "switch_id must be non-negative"})
		return
	}
	result, err := client.Call(r.Context(), "Switch.Toggle", map[string]any{"id": switchID, "tag": "go-web"})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	// Switch.Toggle answers with the previous output, so the settled state is
	// simply its inverse.
	var toggled struct {
		WasOn *bool `json:"was_on"`
	}
	var expected *bool
	if json.Unmarshal(result, &toggled) == nil && toggled.WasOn != nil {
		want := !*toggled.WasOn
		expected = &want
	}
	status, statusErr := settledSwitchStatus(r.Context(), client, switchID, expected)
	response := map[string]any{"ok": true, "rpc_result": result}
	if statusErr != nil {
		response["status_error"] = statusErr.Error()
	} else {
		response["switch_status"] = status
	}
	writeJSON(w, http.StatusOK, response)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(value)
}

func (a *App) withUIAuth(next http.HandlerFunc) http.HandlerFunc {
	if a.cfg.UIPassword == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(username), []byte(a.cfg.UIUsername)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(password), []byte(a.cfg.UIPassword)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Shelly controller", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
