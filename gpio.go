package main

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/warthog618/go-gpiocdev"
)

const (
	biasPullUp   = "pull-up"
	biasPullDown = "pull-down"
	biasNone     = "disabled"

	eventHistorySize = 200
	subscriberBuffer = 64
)

// PinConfig is the electrical configuration applied to a single input line.
type PinConfig struct {
	Bias     string        `json:"bias"`
	Debounce time.Duration `json:"-"`
}

// PinState is the live view of one GPIO line, as shown by the visualizer.
type PinState struct {
	GPIO        int    `json:"gpio"`
	PhysicalPin int    `json:"physical_pin"`
	Label       string `json:"label"`
	Note        string `json:"note,omitempty"`
	Available   bool   `json:"available"`
	Error       string `json:"error,omitempty"`
	Bias        string `json:"bias"`
	DebounceMS  int    `json:"debounce_ms"`
	Level       int    `json:"level"`
	Changes     uint64 `json:"changes"`
	LastChange  string `json:"last_change,omitempty"`
}

// GPIOEvent is a debounced edge on one line.
type GPIOEvent struct {
	Seq   uint64 `json:"seq"`
	GPIO  int    `json:"gpio"`
	Level int    `json:"level"`
	Edge  string `json:"edge"`
	At    string `json:"at"`
}

// GPIOWatcher owns every header line as a debounced edge-detecting input and
// fans events out to the button engine and to any number of UI streams.
//
// It deliberately requests each line separately: one line held by another
// driver then costs us that line only, rather than the whole request, and it
// lets bias and debounce be reconfigured per pin at runtime.
type GPIOWatcher struct {
	chipName string
	consumer string

	mu      sync.RWMutex
	chip    *gpiocdev.Chip
	lines   map[int]*gpiocdev.Line
	states  map[int]*PinState
	cfgs    map[int]PinConfig
	order   []int
	history []GPIOEvent
	subs    map[int]chan GPIOEvent
	nextSub int
	seq     uint64
	closed  bool
}

// NewGPIOWatcher opens the chip and claims every offset it can.
// A line that cannot be claimed is reported as unavailable rather than fatal.
func NewGPIOWatcher(chipName, consumer string, offsets []int, defaults PinConfig, overrides map[int]PinConfig) (*GPIOWatcher, error) {
	chip, err := gpiocdev.NewChip(chipName, gpiocdev.WithConsumer(consumer))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", chipName, err)
	}

	w := &GPIOWatcher{
		chipName: chipName,
		consumer: consumer,
		chip:     chip,
		lines:    make(map[int]*gpiocdev.Line, len(offsets)),
		states:   make(map[int]*PinState, len(offsets)),
		cfgs:     make(map[int]PinConfig, len(offsets)),
		order:    append([]int(nil), offsets...),
		subs:     make(map[int]chan GPIOEvent),
	}
	sort.Ints(w.order)

	for _, offset := range w.order {
		cfg := defaults
		if o, ok := overrides[offset]; ok {
			cfg = o
		}
		w.cfgs[offset] = normalizePinConfig(cfg)
		w.states[offset] = newPinState(offset, w.cfgs[offset])
		if err := w.claim(offset); err != nil {
			w.states[offset].Available = false
			w.states[offset].Error = err.Error()
			log.Printf("gpio: line %d unavailable: %v", offset, err)
		}
	}
	return w, nil
}

func newPinState(offset int, cfg PinConfig) *PinState {
	state := &PinState{
		GPIO:        offset,
		PhysicalPin: physicalPinFor(offset),
		Label:       fmt.Sprintf("GPIO%d", offset),
		Bias:        cfg.Bias,
		DebounceMS:  int(cfg.Debounce / time.Millisecond),
	}
	for _, p := range headerPins {
		if p.Kind == "gpio" && p.GPIO == offset {
			state.Label = p.Label
			state.Note = p.Note
			break
		}
	}
	return state
}

func normalizePinConfig(cfg PinConfig) PinConfig {
	switch cfg.Bias {
	case biasPullUp, biasPullDown, biasNone:
	default:
		cfg.Bias = biasPullUp
	}
	if cfg.Debounce < 0 || cfg.Debounce > 2*time.Second {
		cfg.Debounce = 20 * time.Millisecond
	}
	return cfg
}

func biasOption(bias string) gpiocdev.LineReqOption {
	switch bias {
	case biasPullDown:
		return gpiocdev.WithPullDown
	case biasNone:
		return gpiocdev.WithBiasDisabled
	default:
		return gpiocdev.WithPullUp
	}
}

// claim requests one line as a debounced both-edge input. Caller holds no lock
// on first use (construction) but must hold w.mu when reconfiguring.
func (w *GPIOWatcher) claim(offset int) error {
	cfg := w.cfgs[offset]
	opts := []gpiocdev.LineReqOption{
		gpiocdev.AsInput,
		biasOption(cfg.Bias),
		gpiocdev.WithBothEdges,
		gpiocdev.WithEventHandler(w.onEvent),
	}
	if cfg.Debounce > 0 {
		opts = append(opts, gpiocdev.WithDebounce(cfg.Debounce))
	}

	line, err := w.chip.RequestLine(offset, opts...)
	if err != nil && cfg.Debounce > 0 {
		// Kernel debounce needs uAPI v2; fall back rather than lose the line.
		line, err = w.chip.RequestLine(offset,
			gpiocdev.AsInput,
			biasOption(cfg.Bias),
			gpiocdev.WithBothEdges,
			gpiocdev.WithEventHandler(w.onEvent),
		)
		if err == nil {
			log.Printf("gpio: line %d claimed without kernel debounce", offset)
			cfg.Debounce = 0
			w.cfgs[offset] = cfg
			w.states[offset].DebounceMS = 0
		}
	}
	if err != nil {
		return err
	}

	value, valErr := line.Value()
	if valErr != nil {
		_ = line.Close()
		return fmt.Errorf("read initial value: %w", valErr)
	}

	w.lines[offset] = line
	state := w.states[offset]
	state.Available = true
	state.Error = ""
	state.Bias = cfg.Bias
	state.DebounceMS = int(cfg.Debounce / time.Millisecond)
	state.Level = value
	return nil
}

// onEvent runs on gpiocdev's per-line reader goroutine. It must not block, so
// every subscriber send is best-effort.
func (w *GPIOWatcher) onEvent(evt gpiocdev.LineEvent) {
	level := 0
	edge := "falling"
	if evt.Type == gpiocdev.LineEventRisingEdge {
		level = 1
		edge = "rising"
	}
	now := time.Now()

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	state, ok := w.states[evt.Offset]
	if !ok {
		w.mu.Unlock()
		return
	}
	state.Level = level
	state.Changes++
	state.LastChange = now.UTC().Format(time.RFC3339Nano)

	w.seq++
	out := GPIOEvent{
		Seq:   w.seq,
		GPIO:  evt.Offset,
		Level: level,
		Edge:  edge,
		At:    state.LastChange,
	}
	w.history = append(w.history, out)
	if len(w.history) > eventHistorySize {
		w.history = w.history[len(w.history)-eventHistorySize:]
	}
	targets := make([]chan GPIOEvent, 0, len(w.subs))
	for _, ch := range w.subs {
		targets = append(targets, ch)
	}
	w.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- out:
		default: // slow consumer: drop rather than stall the reader goroutine
		}
	}
}

// Subscribe returns a channel of events plus a cancel func. The channel is
// closed only by the cancel func, never by the watcher.
func (w *GPIOWatcher) Subscribe() (<-chan GPIOEvent, func()) {
	ch := make(chan GPIOEvent, subscriberBuffer)
	w.mu.Lock()
	id := w.nextSub
	w.nextSub++
	w.subs[id] = ch
	w.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.subs, id)
			w.mu.Unlock()
			close(ch)
		})
	}
}

// Snapshot returns every watched line in ascending GPIO order.
func (w *GPIOWatcher) Snapshot() []PinState {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]PinState, 0, len(w.order))
	for _, offset := range w.order {
		out = append(out, *w.states[offset])
	}
	return out
}

// History returns the most recent edges, oldest first.
func (w *GPIOWatcher) History() []GPIOEvent {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return append([]GPIOEvent(nil), w.history...)
}

// Level reports the current raw electrical level of a line.
func (w *GPIOWatcher) Level(offset int) (int, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	state, ok := w.states[offset]
	if !ok || !state.Available {
		return 0, false
	}
	return state.Level, true
}

// Configure re-requests a line with new bias and/or debounce. The line is
// briefly released, so edges during the swap are lost.
func (w *GPIOWatcher) Configure(offset int, cfg PinConfig) error {
	cfg = normalizePinConfig(cfg)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("watcher is closed")
	}
	if _, ok := w.states[offset]; !ok {
		return fmt.Errorf("GPIO %d is not watched", offset)
	}

	previous := w.cfgs[offset]
	if line, ok := w.lines[offset]; ok {
		_ = line.Close()
		delete(w.lines, offset)
	}

	w.cfgs[offset] = cfg
	if err := w.claim(offset); err != nil {
		w.cfgs[offset] = previous
		w.states[offset].Available = false
		w.states[offset].Error = err.Error()
		if reErr := w.claim(offset); reErr != nil {
			return fmt.Errorf("apply config: %w (line could not be restored: %v)", err, reErr)
		}
		return fmt.Errorf("apply config: %w", err)
	}
	return nil
}

// ResetCounters zeroes the per-line edge counters shown in the visualizer.
func (w *GPIOWatcher) ResetCounters() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, state := range w.states {
		state.Changes = 0
		state.LastChange = ""
	}
	w.history = nil
}

// Close releases every claimed line and the chip.
func (w *GPIOWatcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	for offset, line := range w.lines {
		_ = line.Close()
		delete(w.lines, offset)
	}
	chip := w.chip
	w.chip = nil
	w.mu.Unlock()

	if chip != nil {
		return chip.Close()
	}
	return nil
}
