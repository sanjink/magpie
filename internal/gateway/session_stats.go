package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessions"
	"github.com/yetone/magpie/internal/usage"
)

const (
	sessionStatsCacheTTL = 900 * time.Millisecond
	sessionStatsPollTTL  = 750 * time.Millisecond
	sessionStatsKept     = 64
)

// SessionTokenStats is the token total of one conversation.
type SessionTokenStats struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// SessionCacheStats repeats the cache columns for clients that draw them as a
// separate card.
type SessionCacheStats struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

// SessionSpeedPoint is one completed model call on the session curve.
type SessionSpeedPoint struct {
	At          time.Time `json:"at"`
	CallID      string    `json:"call_id,omitempty"`
	TPS         float64   `json:"tps"`
	Model       string    `json:"model,omitempty"`
	Provisional bool      `json:"provisional,omitempty"`
}

// SessionStats is the current conversation's analytics. Completed calls only:
// the live call is returned separately as Provisional and never changes any
// completed total or the peak.
type SessionStats struct {
	Session      string              `json:"session"`
	ModelMS      int64               `json:"model_ms"`
	ToolMS       int64               `json:"tool_ms"`
	TTFTMS       *int64              `json:"ttft_ms"`
	TTFTCount    int                 `json:"ttft_count"`
	DecodeTokens int                 `json:"decode_tokens"`
	DecodeMS     int64               `json:"decode_ms"`
	TPS          *float64            `json:"tps"`
	PeakTPS      *float64            `json:"peak_tps"`
	Steps        int                 `json:"steps"`
	ActiveMS     int64               `json:"active_ms"`
	Tokens       SessionTokenStats   `json:"tokens"`
	Cache        SessionCacheStats   `json:"cache"`
	Series       []SessionSpeedPoint `json:"series"`
	Provisional  *SessionSpeedPoint  `json:"provisional"`
}

type sessionInterval struct {
	start time.Time
	end   time.Time
}

type sessionToolSample struct {
	start time.Time
	end   time.Time
}

type sessionStatsEntry struct {
	at    time.Time
	stats SessionStats
}

// sessionStatsCache keeps the expensive trace scan incremental and reuses a
// completed ledger answer across the HUD's sub-second redraws. The HUD polls
// once a second; the cache is intentionally shorter than that interval.
type sessionStatsCache struct {
	mu       sync.Mutex
	trace    *sessions.TraceReader
	tools    map[string]map[string]sessionToolSample
	toolLast map[string]time.Time
	order    []string
	entries  map[string]sessionStatsEntry
	lastPoll time.Time
}

func (c *sessionStatsCache) get(session string, now time.Time) SessionStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pollToolsLocked(now)
	if entry, ok := c.entries[session]; ok && now.Sub(entry.at) < sessionStatsCacheTTL {
		return entry.stats
	}
	if c.entries == nil {
		c.entries = map[string]sessionStatsEntry{}
	}

	ledger := usage.LedgerOf(usage.All, usage.Filter{Session: session})
	tools := make([]sessionToolSample, 0, len(c.tools[session]))
	for _, sample := range c.tools[session] {
		tools = append(tools, sample)
	}
	stats := buildSessionStats(session, ledger.Rows, tools)
	c.entries[session] = sessionStatsEntry{at: now, stats: stats}
	return stats
}

func (c *sessionStatsCache) pollToolsLocked(now time.Time) {
	if !c.lastPoll.IsZero() && now.Sub(c.lastPoll) < sessionStatsPollTTL {
		return
	}
	if c.trace == nil {
		// The HUD is looking at a conversation that may have started before
		// magpie did. Read existing tool completions once, then follow appends
		// incrementally through the same reader.
		c.trace = sessions.NewTraceReader(time.Time{})
	}
	c.lastPoll = now
	spans := c.trace.Poll(false)
	if c.tools == nil {
		c.tools = map[string]map[string]sessionToolSample{}
		c.toolLast = map[string]time.Time{}
	}
	for _, span := range spans {
		if span.Kind != "tool" || span.Session == "" ||
			span.Start.IsZero() || span.End.IsZero() || span.End.Before(span.Start) {
			continue
		}
		tools := c.tools[span.Session]
		if tools == nil {
			tools = map[string]sessionToolSample{}
			c.tools[span.Session] = tools
			c.order = append(c.order, span.Session)
		}
		old, exists := tools[span.ID]
		next := sessionToolSample{start: span.Start, end: span.End}
		if exists && !next.end.After(old.end) {
			continue
		}
		tools[span.ID] = next
		c.toolLast[span.Session] = now
		delete(c.entries, span.Session)
	}
	for len(c.order) > sessionStatsKept {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.tools, oldest)
		delete(c.toolLast, oldest)
		delete(c.entries, oldest)
	}
}

func buildSessionStats(session string, rows []usage.Row, tools []sessionToolSample) SessionStats {
	out := SessionStats{
		Session: session,
		Tokens:  SessionTokenStats{},
		Cache:   SessionCacheStats{},
		Series:  []SessionSpeedPoint{},
	}
	var intervals []sessionInterval
	var ttftSum int64
	for _, row := range rows {
		r := row.Record
		if r.Session != session && r.NativeSession != session {
			continue
		}
		if r.IsRejected() {
			continue
		}
		out.Steps++
		out.ModelMS += max(0, r.Millis)
		out.Tokens.Input += max(0, r.Input)
		out.Tokens.Output += max(0, r.Output)
		out.Tokens.CacheRead += max(0, r.CacheRead)
		out.Tokens.CacheWrite += max(0, r.CacheWrite)
		if !r.Time.IsZero() {
			end := r.Time.Add(time.Duration(max(0, r.Millis)) * time.Millisecond)
			intervals = append(intervals, sessionInterval{start: r.Time, end: end})
		}
		if row.Failed() {
			continue
		}
		if r.TTFT > 0 {
			out.TTFTCount++
			ttftSum += r.TTFT
		}
		tokens, window := row.Decode()
		if tokens <= 0 || window <= 0 {
			continue
		}
		speed := float64(tokens) / (float64(window) / 1000)
		if !validSessionTPS(speed) {
			continue
		}
		out.DecodeTokens += tokens
		out.DecodeMS += window
		at := r.Time
		if !at.IsZero() {
			at = at.Add(time.Duration(max(0, r.Millis)) * time.Millisecond)
		}
		if at.IsZero() {
			continue
		}
		model := r.Served
		if model == "" {
			model = r.Model
		}
		out.Series = append(out.Series, SessionSpeedPoint{
			At:     at,
			CallID: sessionCallID(r),
			TPS:    speed,
			Model:  model,
		})
	}

	for _, tool := range tools {
		if tool.start.IsZero() || tool.end.IsZero() || tool.end.Before(tool.start) {
			continue
		}
		out.ToolMS += tool.end.Sub(tool.start).Milliseconds()
		intervals = append(intervals, sessionInterval{start: tool.start, end: tool.end})
	}
	out.Cache = SessionCacheStats{Read: out.Tokens.CacheRead, Write: out.Tokens.CacheWrite}
	if out.TTFTCount > 0 {
		ttft := ttftSum / int64(out.TTFTCount)
		out.TTFTMS = &ttft
	}
	if out.DecodeTokens > 0 && out.DecodeMS > 0 {
		tps := float64(out.DecodeTokens) / (float64(out.DecodeMS) / 1000)
		if validSessionTPS(tps) {
			out.TPS = &tps
		}
	}
	sort.SliceStable(out.Series, func(i, j int) bool { return out.Series[i].At.Before(out.Series[j].At) })
	for i := range out.Series {
		if out.PeakTPS == nil || out.Series[i].TPS > *out.PeakTPS {
			v := out.Series[i].TPS
			out.PeakTPS = &v
		}
	}
	out.ActiveMS = mergedSessionMS(intervals)
	return out
}

func validSessionTPS(v float64) bool {
	return v > 0 && v <= usage.MaxDecodeSpeed
}

func sessionCallID(r usage.Record) string {
	switch {
	case r.ResponseID != "":
		return r.ResponseID
	case r.RequestID != "":
		return r.RequestID
	case r.RouteID != 0:
		return fmt.Sprintf("route-%d", r.RouteID)
	}
	return strings.Join([]string{r.Agent, r.Model, r.Time.UTC().Format(time.RFC3339Nano)}, ":")
}

func mergedSessionMS(intervals []sessionInterval) int64 {
	if len(intervals) == 0 {
		return 0
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start.Before(intervals[j].start) })
	var total time.Duration
	start, end := intervals[0].start, intervals[0].end
	for _, interval := range intervals[1:] {
		if interval.start.After(end) {
			total += end.Sub(start)
			start, end = interval.start, interval.end
			continue
		}
		if interval.end.After(end) {
			end = interval.end
		}
	}
	total += end.Sub(start)
	return max(0, total.Milliseconds())
}

func withSessionProvisional(stats SessionStats, live liveSnapshot, now time.Time) SessionStats {
	if !live.Active || live.Done || live.Stale || !validSessionTPS(live.Speed) {
		return stats
	}
	at := now
	if live.UpdatedAt != nil {
		at = *live.UpdatedAt
	}
	stats.Provisional = &SessionSpeedPoint{
		At:          at,
		CallID:      "live",
		TPS:         live.Speed,
		Model:       live.Model,
		Provisional: true,
	}
	return stats
}

// sessionStats answers GET /v1/magpie/session/stats?session=<id>. It is a
// loopback-only, read-only analytics endpoint: no prompt, reply, tool body or
// credential is read or returned.
func (s *Server) sessionStats(w http.ResponseWriter, r *http.Request) {
	if !local(r) {
		writeError(w, provider.Chat, http.StatusForbidden, "magpie's session stats are told only to a program on this computer over loopback")
		return
	}
	session := strings.TrimSpace(r.URL.Query().Get("session"))
	if len(session) > 128 {
		session = session[:128]
	}
	if session == "" {
		session = sessionOf(r.Header)
	}
	if session == "" {
		writeError(w, provider.Chat, http.StatusBadRequest, "name the session: ?session=<id> or the "+SessionHeader+" header")
		return
	}
	now := time.Now()
	stats := s.sessionStatsCache.get(session, now)
	stats = withSessionProvisional(stats, s.liveState.snapshot(session, "", now), now)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, stats)
}
