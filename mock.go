package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mock is a pretend GP2040-CE controller (stock Pico pinout) for demo mode.
// Simulated button presses arrive through /mock/press?pin=N.
type mock struct {
	mu      sync.Mutex
	doc     map[string]any
	presses chan int
	abort   chan struct{}
}

func newMock() *mock {
	pico := map[int]int{
		0: 0, 1: 0, 15: 0, 28: 0, // add-ons
		2: 1, 3: 2, 4: 4, 5: 3, // up down right left
		6: 5, 7: 6, 8: 12, 9: 11, 10: 7, 11: 8, 12: 10, 13: 9,
		14: 32, 16: 13, 17: 14, 18: 17, 19: 18, 20: 15, 21: 16,
	}
	doc := map[string]any{}
	for p := 0; p < 30; p++ {
		a, ok := pico[p]
		if !ok {
			a = -10
		}
		doc[fmt.Sprintf("pin%02d", p)] = map[string]any{"action": a, "customButtonMask": 0, "customDpadMask": 0}
	}
	doc["profileLabel"] = ""
	doc["enabled"] = true
	doc["pinInversionEnabled"] = false
	doc["invertedPins"] = []int{}
	return &mock{doc: doc, presses: make(chan int, 8), abort: make(chan struct{}, 1)}
}

func (m *mock) press(w http.ResponseWriter, r *http.Request) {
	pin, err := strconv.Atoi(r.URL.Query().Get("pin"))
	if err != nil || pin < 0 || pin > 29 {
		http.Error(w, "bad pin", 400)
		return
	}
	select {
	case m.presses <- pin:
	default:
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, "/api/") {
	case "getFirmwareVersion":
		writeJSON(w, map[string]string{"version": "v0.7.12 (demo)", "boardConfigLabel": "Pico (pretend controller)", "boardConfig": "Pico"})
	case "getGamepadOptions":
		writeJSON(w, map[string]any{"inputMode": 0})
	case "getPinMappings":
		m.mu.Lock()
		b, _ := json.Marshal(m.doc)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	case "setPinMappings":
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		if json.Unmarshal(body, &in) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		m.mu.Lock()
		for k, v := range in {
			if strings.HasPrefix(k, "pin") {
				old, _ := m.doc[k].(map[string]any)
				nv, _ := v.(map[string]any)
				oa, na := num(old["action"]), num(nv["action"])
				// Same rule as the firmware: reserved / add-on pins never change.
				if oa == -5 || oa == 0 || na == -5 || na == 0 {
					continue
				}
			}
			m.doc[k] = v
		}
		m.mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	case "getHeldPins":
		// drop stale presses, then wait up to 5 s like the real firmware
		for len(m.presses) > 0 {
			<-m.presses
		}
		select {
		case <-m.abort:
		default:
		}
		select {
		case p := <-m.presses:
			time.Sleep(120 * time.Millisecond)
			writeJSON(w, map[string]any{"heldPins": []int{p}})
		case <-m.abort:
		case <-time.After(5 * time.Second):
			writeJSON(w, map[string]any{"heldPins": []int{}})
		case <-r.Context().Done():
		}
	case "abortGetHeldPins":
		select {
		case m.abort <- struct{}{}:
		default:
		}
	case "reboot":
		writeJSON(w, map[string]bool{"success": true})
	default:
		http.Error(w, "not in demo", 404)
	}
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return -10
}
