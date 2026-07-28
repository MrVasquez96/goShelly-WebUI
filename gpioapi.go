package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

//go:embed gpio.html
var gpioHTML string

type gpioOverview struct {
	Available bool        `json:"available"`
	Chip      string      `json:"chip,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	Header    []HeaderPin `json:"header"`
	Pins      []PinState  `json:"pins"`
	History   []GPIOEvent `json:"history"`
}

func (a *App) handleGPIOPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/gpio" {
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
	_, _ = w.Write([]byte(gpioHTML))
}

func (a *App) handleGPIOState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	overview := gpioOverview{Header: headerPins}
	if a.gpio == nil {
		overview.Reason = a.gpioReason
		overview.Pins = []PinState{}
		overview.History = []GPIOEvent{}
	} else {
		overview.Available = true
		overview.Chip = a.gpio.chipName
		overview.Pins = a.gpio.Snapshot()
		overview.History = a.gpio.History()
	}
	writeJSON(w, http.StatusOK, overview)
}

// handleGPIOStream pushes edges to the browser over SSE. The server's global
// WriteTimeout would kill a long-lived stream, so the deadline is cleared here
// and liveness is maintained with periodic comment frames instead.
func (a *App) handleGPIOStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if a.gpio == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GPIO is not available: " + a.gpioReason})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, unsubscribe := a.gpio.Subscribe()
	defer unsubscribe()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case evt, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: edge\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *App) handleGPIOConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !requireControlHeader(w, r) {
		return
	}
	if a.gpio == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GPIO is not available: " + a.gpioReason})
		return
	}

	var input struct {
		GPIO       *int    `json:"gpio"`
		Bias       *string `json:"bias,omitempty"`
		DebounceMS *int    `json:"debounce_ms,omitempty"`
		All        bool    `json:"all,omitempty"`
	}
	if err := decodeJSONBody(w, r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !input.All && input.GPIO == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "field 'gpio' is required unless 'all' is true"})
		return
	}

	bias := a.pinDefaults.Bias
	if input.Bias != nil {
		switch *input.Bias {
		case biasPullUp, biasPullDown, biasNone:
			bias = *input.Bias
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bias must be pull-up, pull-down or disabled"})
			return
		}
	}
	debounce := a.pinDefaults.Debounce
	if input.DebounceMS != nil {
		if *input.DebounceMS < 0 || *input.DebounceMS > 2000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "debounce_ms must be between 0 and 2000"})
			return
		}
		debounce = time.Duration(*input.DebounceMS) * time.Millisecond
	}
	cfg := PinConfig{Bias: bias, Debounce: debounce}

	failures := make(map[string]string)
	if input.All {
		for _, state := range a.gpio.Snapshot() {
			if err := a.gpio.Configure(state.GPIO, cfg); err != nil {
				failures[fmt.Sprintf("gpio%d", state.GPIO)] = err.Error()
			}
		}
	} else if err := a.gpio.Configure(*input.GPIO, cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	response := map[string]any{"ok": true, "pins": a.gpio.Snapshot()}
	if len(failures) > 0 {
		response["failures"] = failures
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) handleGPIOReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !requireControlHeader(w, r) {
		return
	}
	if a.gpio == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GPIO is not available: " + a.gpioReason})
		return
	}
	a.gpio.ResetCounters()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pins": a.gpio.Snapshot()})
}

type deviceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

func (a *App) handleBindings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.writeBindings(w, http.StatusOK)
	case http.MethodPut:
		if !requireControlHeader(w, r) {
			return
		}
		if a.buttons == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GPIO is not available: " + a.gpioReason})
			return
		}
		var input struct {
			Bindings []Binding `json:"bindings"`
		}
		if err := decodeJSONBody(w, r, &input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := a.buttons.Replace(input.Bindings); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.writeBindings(w, http.StatusOK)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (a *App) writeBindings(w http.ResponseWriter, status int) {
	devices := make([]deviceRef, 0, len(a.cfg.Devices))
	for _, d := range a.cfg.Devices {
		devices = append(devices, deviceRef{ID: d.ID, Name: d.Name, URL: d.URL})
	}
	payload := map[string]any{
		"devices":  devices,
		"modes":    []string{ModeToggle, ModeMomentary, ModeMomentaryInverse, ModeOn, ModeOff},
		"bindings": []BindingStatus{},
	}
	if a.buttons == nil {
		payload["available"] = false
		payload["reason"] = a.gpioReason
	} else {
		payload["available"] = true
		payload["bindings"] = a.buttons.Status()
	}
	writeJSON(w, status, payload)
}

func requireControlHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Shelly-Control") != "1" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing X-Shelly-Control: 1 header"})
		return false
	}
	return true
}
