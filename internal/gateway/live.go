package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/provider"
)

const (
	liveKept           = 64
	liveDoneKeep       = 30 * time.Second
	liveStaleAfter     = 1500 * time.Millisecond
	liveSpeedWindow    = time.Second
	liveSpeedMinWindow = 300 * time.Millisecond
	liveSpeedMinTokens = 8
	liveSpeedMax       = 10_000.0
)

type livePhase string

const (
	liveWaiting   livePhase = "waiting"
	liveReasoning livePhase = "reasoning"
	liveText      livePhase = "text"
	liveTool      livePhase = "tool"
)

type liveDelta struct {
	phase livePhase
	text  string
}

type liveSample struct {
	at     time.Time
	tokens int
}

type liveRun struct {
	hub       *liveHub
	id        uint64
	session   string
	turn      string
	model     string
	phase     livePhase
	started   time.Time
	first     time.Time
	updated   time.Time
	estimated int
	samples   []liveSample
	done      bool
	completed time.Time
}

func (r *liveRun) observeAll(deltas []liveDelta, at time.Time) {
	if r == nil || len(deltas) == 0 {
		return
	}
	for _, delta := range deltas {
		r.hub.observe(r, delta, at)
	}
}

type liveHub struct {
	mu    sync.Mutex
	next  uint64
	runs  map[uint64]*liveRun
	order []uint64
}

type liveSnapshot struct {
	Active          bool       `json:"active"`
	Done            bool       `json:"done"`
	Session         string     `json:"session"`
	Turn            string     `json:"turn"`
	Model           string     `json:"model"`
	Phase           string     `json:"phase"`
	StartedAt       *time.Time `json:"started_at"`
	FirstAt         *time.Time `json:"first_at"`
	UpdatedAt       *time.Time `json:"updated_at"`
	EstimatedTokens int        `json:"estimated_tokens"`
	Speed           float64    `json:"speed"`
	WindowMS        int64      `json:"window_ms"`
	SpeedEstimated  bool       `json:"speed_estimated"`
	Stale           bool       `json:"stale"`
}

type liveRunKey struct{}

func (h *liveHub) begin(session, turn, model string, at time.Time) *liveRun {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.pruneLocked(at)
	if h.runs == nil {
		h.runs = make(map[uint64]*liveRun)
	}
	h.next++
	run := &liveRun{
		hub:     h,
		id:      h.next,
		session: session,
		turn:    turn,
		model:   model,
		phase:   liveWaiting,
		started: at,
		updated: at,
	}
	h.runs[run.id] = run
	h.order = append(h.order, run.id)
	h.trimLocked()
	return run
}

func (h *liveHub) observe(run *liveRun, delta liveDelta, at time.Time) {
	if run == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runs[run.id] != run || run.done {
		return
	}
	if at.Before(run.updated) {
		at = run.updated
	}
	if delta.phase != "" {
		run.phase = delta.phase
		if run.first.IsZero() {
			run.first = at
		}
	}
	if tokens := liveTokens(delta.text); tokens > 0 {
		run.estimated += tokens
		run.samples = append(run.samples, liveSample{at: at, tokens: tokens})
	}
	run.updated = at
	run.trimSamples()
}

func (h *liveHub) finish(run *liveRun, at time.Time) {
	if run == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runs[run.id] != run || run.done {
		return
	}
	if at.Before(run.updated) {
		at = run.updated
	}
	run.done = true
	run.completed = at
	run.updated = at
}

func (h *liveHub) snapshot(session, turn string, at time.Time) liveSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.pruneLocked(at)
	var chosen *liveRun
	for _, id := range h.order {
		run := h.runs[id]
		if run == nil || run.session != session {
			continue
		}
		if turn != "" && run.turn != "" && run.turn != turn {
			continue
		}
		if chosen == nil || !run.done && chosen.done || run.done == chosen.done && run.id > chosen.id {
			chosen = run
		}
	}
	if chosen == nil {
		return liveSnapshot{
			Session:        session,
			Turn:           turn,
			Phase:          string(liveWaiting),
			WindowMS:       liveSpeedWindow.Milliseconds(),
			SpeedEstimated: true,
		}
	}

	stale := !chosen.done && at.Sub(chosen.updated) > liveStaleAfter
	speed := chosen.speedLocked()
	if stale {
		speed = 0
	}
	started, updated := chosen.started, chosen.updated
	out := liveSnapshot{
		Active:          !chosen.done,
		Done:            chosen.done,
		Session:         chosen.session,
		Turn:            chosen.turn,
		Model:           chosen.model,
		Phase:           string(chosen.phase),
		StartedAt:       &started,
		UpdatedAt:       &updated,
		EstimatedTokens: chosen.estimated,
		Speed:           speed,
		WindowMS:        liveSpeedWindow.Milliseconds(),
		SpeedEstimated:  true,
		Stale:           stale,
	}
	if !chosen.first.IsZero() {
		first := chosen.first
		out.FirstAt = &first
	}
	return out
}

func (h *liveHub) pruneLocked(now time.Time) {
	if len(h.order) == 0 {
		return
	}
	kept := h.order[:0]
	for _, id := range h.order {
		run := h.runs[id]
		if run == nil || run.done && now.Sub(run.completed) > liveDoneKeep {
			delete(h.runs, id)
			continue
		}
		kept = append(kept, id)
	}
	h.order = kept
	h.trimLocked()
}

func (h *liveHub) trimLocked() {
	for len(h.order) > liveKept {
		id := h.order[0]
		h.order = h.order[1:]
		delete(h.runs, id)
	}
}

func (r *liveRun) trimSamples() {
	cutoff := r.updated.Add(-liveSpeedWindow)
	first := 0
	for first < len(r.samples) && r.samples[first].at.Before(cutoff) {
		first++
	}
	if first > 0 {
		copy(r.samples, r.samples[first:])
		r.samples = r.samples[:len(r.samples)-first]
	}
}

func (r *liveRun) speedLocked() float64 {
	if len(r.samples) == 0 {
		return 0
	}
	span := r.samples[len(r.samples)-1].at.Sub(r.samples[0].at)
	if span < liveSpeedMinWindow {
		return 0
	}
	tokens := 0
	for _, sample := range r.samples {
		tokens += sample.tokens
	}
	if tokens < liveSpeedMinTokens {
		return 0
	}
	speed := float64(tokens) / span.Seconds()
	return min(speed, liveSpeedMax)
}

// liveTokens estimates streamed tokens without storing the text: four ASCII
// characters per token, each non-ASCII character as one, and no empty delta
// below one token.
func liveTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii, nonASCII := 0, 0
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		if r < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII++
		}
		text = text[size:]
	}
	return max(ascii/4+nonASCII, 1)
}

func liveRunOf(r *http.Request) *liveRun {
	run, _ := r.Context().Value(liveRunKey{}).(*liveRun)
	return run
}

// beginLiveRun registers the one live row a request publishes. Calling it
// again for the same request is a no-op, as serveAgent and the native Codex
// path can meet at the same handler boundary.
func (s *Server) beginLiveRun(r *http.Request, body []byte, at time.Time) (*http.Request, func()) {
	if liveRunOf(r) != nil || r.Method != http.MethodPost {
		return r, func() {}
	}
	metadata := requestSessionMetadata(r.Header, body)
	kind := requestCallKind(r.Header, metadata)
	if !liveTracks(r, body, metadata, kind) {
		return r, func() {}
	}
	run := s.liveState.begin(sessionOf(r.Header), metadata.Turn, unprefixed(modelOf(body)), at)
	ctx := context.WithValue(r.Context(), liveRunKey{}, run)
	return r.WithContext(ctx), func() {
		s.liveState.finish(run, time.Now())
	}
}

func liveTracks(r *http.Request, body []byte, metadata sessionMetadata, kind string) bool {
	if agentOf(r) != "codex" || sessionOf(r.Header) == "" || !streamOf(body) {
		return false
	}
	if kind != "" || isTitleKind(kind) || isDescriptionKind(kind) || isPredictionKind(kind) ||
		subOf(r.Header) != "" || r.Header.Get("x-openai-memgen-request") != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(metadata.Source)) {
	case "", "user":
		return true
	default:
		return false
	}
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	if !local(r) {
		writeError(w, provider.Chat, http.StatusForbidden, "magpie's live stream is told only to a program on this computer over loopback")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	session := strings.TrimSpace(r.URL.Query().Get("session"))
	turn := strings.TrimSpace(r.URL.Query().Get("turn"))
	writeJSON(w, http.StatusOK, s.liveState.snapshot(session, turn, time.Now()))
}
