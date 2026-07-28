package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Button behaviours. These are what the "mode" field of a binding may hold.
const (
	// ModeToggle flips the relay once per press. Releasing does nothing.
	ModeToggle = "toggle"
	// ModeMomentary closes the relay while held and opens it on release.
	ModeMomentary = "momentary"
	// ModeMomentaryInverse opens the relay while held and closes it on release.
	ModeMomentaryInverse = "momentary_inverse"
	// ModeOn drives the relay on at every press.
	ModeOn = "on"
	// ModeOff drives the relay off at every press.
	ModeOff = "off"
)

const bindingActionQueue = 8

var bindingIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Binding maps one physical button to one relay.
type Binding struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	GPIO       int    `json:"gpio"`
	DeviceID   string `json:"device_id"`
	SwitchID   int    `json:"switch_id"`
	Mode       string `json:"mode"`
	ActiveLow  bool   `json:"active_low"`
	Enabled    bool   `json:"enabled"`
	Bias       string `json:"bias"`
	DebounceMS int    `json:"debounce_ms"`
}

// BindingStatus is the live outcome of a binding, surfaced in the UI so a
// miswired button or an unreachable relay is obvious.
type BindingStatus struct {
	Binding
	Pressed    bool   `json:"pressed"`
	Fires      uint64 `json:"fires"`
	LastFired  string `json:"last_fired,omitempty"`
	LastAction string `json:"last_action,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	PinOK      bool   `json:"pin_ok"`
}

type bindingsFile struct {
	Bindings []Binding `json:"bindings"`
}

type bindingAction struct {
	toggle bool
	on     bool
	label  string
}

type bindingRuntime struct {
	binding Binding
	actions chan bindingAction
	done    chan struct{}

	mu         sync.Mutex
	pressed    bool
	fires      uint64
	lastFired  string
	lastAction string
	lastError  string
}

// ButtonEngine turns debounced GPIO edges into Shelly RPC calls.
//
// Each binding gets its own worker goroutine so that a slow or failing relay
// call can never reorder a later press/release of the same button, and can
// never block the GPIO reader.
type ButtonEngine struct {
	app      *App
	watcher  *GPIOWatcher
	path     string
	defaults PinConfig
	timeout  time.Duration

	mu       sync.RWMutex
	bindings []Binding
	runtime  map[string]*bindingRuntime

	stop   chan struct{}
	wg     sync.WaitGroup
	closed bool
}

// NewButtonEngine loads bindings from disk and starts dispatching.
// A missing bindings file is not an error; it just means nothing is bound yet.
func NewButtonEngine(app *App, watcher *GPIOWatcher, path string, defaults PinConfig, timeout time.Duration) (*ButtonEngine, error) {
	bindings, err := loadBindings(path)
	if err != nil {
		return nil, err
	}

	e := &ButtonEngine{
		app:      app,
		watcher:  watcher,
		path:     path,
		defaults: defaults,
		timeout:  timeout,
		runtime:  make(map[string]*bindingRuntime),
		stop:     make(chan struct{}),
	}
	if err := e.validateAll(bindings); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	e.bindings = bindings
	e.rebuild()

	e.wg.Add(1)
	go e.dispatch()
	return e, nil
}

func loadBindings(path string) ([]Binding, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Binding{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file bindingsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if file.Bindings == nil {
		file.Bindings = []Binding{}
	}
	return file.Bindings, nil
}

// Bindings returns the configured bindings, sorted for stable display.
func (e *ButtonEngine) Bindings() []Binding {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := append([]Binding(nil), e.bindings...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].GPIO != out[j].GPIO {
			return out[i].GPIO < out[j].GPIO
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Status returns each binding together with its live state.
func (e *ButtonEngine) Status() []BindingStatus {
	bindings := e.Bindings()
	out := make([]BindingStatus, 0, len(bindings))

	e.mu.RLock()
	runtimes := make(map[string]*bindingRuntime, len(e.runtime))
	for id, rt := range e.runtime {
		runtimes[id] = rt
	}
	e.mu.RUnlock()

	for _, b := range bindings {
		status := BindingStatus{Binding: b}
		if level, ok := e.watcher.Level(b.GPIO); ok {
			status.PinOK = true
			status.Pressed = isPressed(level, b.ActiveLow)
		}
		if rt, ok := runtimes[b.ID]; ok {
			rt.mu.Lock()
			status.Fires = rt.fires
			status.LastFired = rt.lastFired
			status.LastAction = rt.lastAction
			status.LastError = rt.lastError
			rt.mu.Unlock()
		}
		out = append(out, status)
	}
	return out
}

// Replace validates, persists and applies a whole new binding set.
func (e *ButtonEngine) Replace(bindings []Binding) error {
	if bindings == nil {
		bindings = []Binding{}
	}
	if err := e.validateAll(bindings); err != nil {
		return err
	}
	if err := saveBindings(e.path, bindings); err != nil {
		return err
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("engine is closed")
	}
	e.bindings = bindings
	e.mu.Unlock()

	e.rebuild()
	e.applyPinConfigs()
	return nil
}

func (e *ButtonEngine) validateAll(bindings []Binding) error {
	seen := make(map[string]struct{}, len(bindings))
	for i := range bindings {
		b := &bindings[i]
		b.ID = strings.TrimSpace(b.ID)
		b.Name = strings.TrimSpace(b.Name)
		b.DeviceID = strings.TrimSpace(b.DeviceID)
		b.Mode = strings.TrimSpace(strings.ToLower(b.Mode))

		if !bindingIDPattern.MatchString(b.ID) {
			return fmt.Errorf("binding[%d]: id must be 1-64 characters of A-Z a-z 0-9 _ -", i)
		}
		if _, dup := seen[b.ID]; dup {
			return fmt.Errorf("duplicate binding id %q", b.ID)
		}
		seen[b.ID] = struct{}{}

		if b.Name == "" {
			b.Name = b.ID
		}
		switch b.Mode {
		case ModeToggle, ModeMomentary, ModeMomentaryInverse, ModeOn, ModeOff:
		default:
			return fmt.Errorf("binding %q: unknown mode %q", b.ID, b.Mode)
		}
		if _, ok := e.app.clients[b.DeviceID]; !ok {
			return fmt.Errorf("binding %q: unknown device %q", b.ID, b.DeviceID)
		}
		if b.SwitchID < 0 {
			return fmt.Errorf("binding %q: switch_id must be non-negative", b.ID)
		}
		if _, ok := e.watcher.Level(b.GPIO); !ok {
			if !e.watcherKnows(b.GPIO) {
				return fmt.Errorf("binding %q: GPIO %d is not on the header", b.ID, b.GPIO)
			}
		}
		switch b.Bias {
		case "":
			b.Bias = e.defaults.Bias
		case biasPullUp, biasPullDown, biasNone:
		default:
			return fmt.Errorf("binding %q: unknown bias %q", b.ID, b.Bias)
		}
		if b.DebounceMS < 0 || b.DebounceMS > 2000 {
			return fmt.Errorf("binding %q: debounce_ms must be between 0 and 2000", b.ID)
		}
		if b.DebounceMS == 0 {
			b.DebounceMS = int(e.defaults.Debounce / time.Millisecond)
		}
	}
	return nil
}

func (e *ButtonEngine) watcherKnows(gpio int) bool {
	for _, state := range e.watcher.Snapshot() {
		if state.GPIO == gpio {
			return true
		}
	}
	return false
}

// applyPinConfigs pushes per-binding bias/debounce onto the watcher. Where two
// bindings share a pin the lowest GPIO-then-ID binding wins, matching the
// display order so the UI never disagrees with the hardware.
func (e *ButtonEngine) applyPinConfigs() {
	applied := make(map[int]struct{})
	for _, b := range e.Bindings() {
		if _, done := applied[b.GPIO]; done {
			continue
		}
		applied[b.GPIO] = struct{}{}
		cfg := PinConfig{Bias: b.Bias, Debounce: time.Duration(b.DebounceMS) * time.Millisecond}
		if err := e.watcher.Configure(b.GPIO, cfg); err != nil {
			log.Printf("buttons: GPIO %d config failed: %v", b.GPIO, err)
		}
	}
}

// rebuild stops the previous workers and starts one per current binding.
func (e *ButtonEngine) rebuild() {
	e.mu.Lock()
	old := e.runtime
	next := make(map[string]*bindingRuntime, len(e.bindings))
	for _, b := range e.bindings {
		rt := &bindingRuntime{
			binding: b,
			actions: make(chan bindingAction, bindingActionQueue),
			done:    make(chan struct{}),
		}
		if prev, ok := old[b.ID]; ok {
			prev.mu.Lock()
			rt.fires = prev.fires
			rt.lastFired = prev.lastFired
			rt.lastAction = prev.lastAction
			rt.lastError = prev.lastError
			rt.mu.Unlock()
		}
		next[b.ID] = rt
	}
	e.runtime = next
	e.mu.Unlock()

	for _, rt := range old {
		close(rt.done)
	}
	for _, rt := range next {
		e.wg.Add(1)
		go e.runWorker(rt)
	}
}

func (e *ButtonEngine) runWorker(rt *bindingRuntime) {
	defer e.wg.Done()
	for {
		select {
		case <-rt.done:
			return
		case <-e.stop:
			return
		case action := <-rt.actions:
			e.execute(rt, action)
		}
	}
}

func (e *ButtonEngine) execute(rt *bindingRuntime, action bindingAction) {
	b := rt.binding
	client, ok := e.app.clients[b.DeviceID]
	if !ok {
		rt.record(action.label, fmt.Errorf("unknown device %q", b.DeviceID))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	var err error
	if action.toggle {
		_, err = client.Call(ctx, "Switch.Toggle", map[string]any{"id": b.SwitchID, "tag": "gpio-button"})
	} else {
		_, err = client.Call(ctx, "Switch.Set", map[string]any{"id": b.SwitchID, "on": action.on, "tag": "gpio-button"})
	}
	rt.record(action.label, err)
	if err != nil {
		log.Printf("buttons: %s (GPIO %d) -> %s %s failed: %v", b.ID, b.GPIO, b.DeviceID, action.label, err)
	}
}

func (rt *bindingRuntime) record(label string, err error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.fires++
	rt.lastFired = time.Now().UTC().Format(time.RFC3339)
	rt.lastAction = label
	if err != nil {
		rt.lastError = err.Error()
	} else {
		rt.lastError = ""
	}
}

// dispatch consumes GPIO edges and converts them into per-binding actions.
func (e *ButtonEngine) dispatch() {
	defer e.wg.Done()
	events, unsubscribe := e.watcher.Subscribe()
	defer unsubscribe()

	for {
		select {
		case <-e.stop:
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			e.handle(evt)
		}
	}
}

func (e *ButtonEngine) handle(evt GPIOEvent) {
	e.mu.RLock()
	targets := make([]*bindingRuntime, 0, 2)
	for _, b := range e.bindings {
		if b.GPIO != evt.GPIO || !b.Enabled {
			continue
		}
		if rt, ok := e.runtime[b.ID]; ok {
			targets = append(targets, rt)
		}
	}
	e.mu.RUnlock()

	for _, rt := range targets {
		b := rt.binding
		pressed := isPressed(evt.Level, b.ActiveLow)

		rt.mu.Lock()
		rt.pressed = pressed
		rt.mu.Unlock()

		action, fire := actionFor(b.Mode, pressed)
		if !fire {
			continue
		}
		select {
		case rt.actions <- action:
		default:
			log.Printf("buttons: %s queue full, dropped %s", b.ID, action.label)
		}
	}
}

// actionFor decides what a mode does on a given edge.
func actionFor(mode string, pressed bool) (bindingAction, bool) {
	switch mode {
	case ModeToggle:
		if pressed {
			return bindingAction{toggle: true, label: "toggle"}, true
		}
	case ModeMomentary:
		if pressed {
			return bindingAction{on: true, label: "on (press)"}, true
		}
		return bindingAction{on: false, label: "off (release)"}, true
	case ModeMomentaryInverse:
		if pressed {
			return bindingAction{on: false, label: "off (press)"}, true
		}
		return bindingAction{on: true, label: "on (release)"}, true
	case ModeOn:
		if pressed {
			return bindingAction{on: true, label: "on"}, true
		}
	case ModeOff:
		if pressed {
			return bindingAction{on: false, label: "off"}, true
		}
	}
	return bindingAction{}, false
}

// isPressed converts a raw electrical level into a logical button state.
// An active-low button idles high through its pull-up and reads 0 when shorted
// to ground, which is how almost every mechanical button is wired.
func isPressed(level int, activeLow bool) bool {
	if activeLow {
		return level == 0
	}
	return level == 1
}

func saveBindings(path string, bindings []Binding) error {
	payload, err := json.MarshalIndent(bindingsFile{Bindings: bindings}, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".bindings-*.json")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(payload); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Close stops dispatching and waits for in-flight relay calls to finish.
func (e *ButtonEngine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	close(e.stop)
	e.mu.Unlock()
	e.wg.Wait()
}
