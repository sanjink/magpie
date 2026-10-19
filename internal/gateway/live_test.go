package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestLiveSpeedWindowAndStale(t *testing.T) {
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	var hub liveHub
	run := hub.begin("session-a", "turn-a", "model-a", base)
	hub.observe(run, liveDelta{phase: liveText, text: "abcdefgh"}, base)
	hub.observe(run, liveDelta{phase: liveText, text: "中文中文中文中文"}, base.Add(400*time.Millisecond))

	got := hub.snapshot("session-a", "turn-a", base.Add(500*time.Millisecond))
	if !got.Active || got.Done || got.Stale {
		t.Fatalf("state active=%v done=%v stale=%v", got.Active, got.Done, got.Stale)
	}
	if got.Phase != string(liveText) || got.EstimatedTokens != 10 {
		t.Fatalf("phase=%q tokens=%d", got.Phase, got.EstimatedTokens)
	}
	if got.FirstAt == nil || !got.FirstAt.Equal(base) {
		t.Fatalf("first_at=%v, want %v", got.FirstAt, base)
	}
	if got.Speed != 25 {
		t.Fatalf("speed=%v, want 25 tok/s", got.Speed)
	}

	stale := hub.snapshot("session-a", "turn-a", base.Add(1901*time.Millisecond))
	if !stale.Active || !stale.Stale || stale.Speed != 0 {
		t.Fatalf("stale=%v active=%v speed=%v", stale.Stale, stale.Active, stale.Speed)
	}

	hub.finish(run, base.Add(2*time.Second))
	done := hub.snapshot("session-a", "turn-a", base.Add(2*time.Second))
	if done.Active || !done.Done || done.Stale {
		t.Fatalf("done active=%v done=%v stale=%v", done.Active, done.Done, done.Stale)
	}
}

func TestLiveSpeedNeedsWindowAndTokens(t *testing.T) {
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	var hub liveHub

	short := hub.begin("short", "", "", base)
	hub.observe(short, liveDelta{phase: liveText, text: strings.Repeat("x", 100)}, base)
	hub.observe(short, liveDelta{phase: liveText, text: strings.Repeat("x", 100)}, base.Add(299*time.Millisecond))
	if got := hub.snapshot("short", "", base.Add(400*time.Millisecond)).Speed; got != 0 {
		t.Fatalf("299ms window speed=%v, want 0", got)
	}

	few := hub.begin("few", "", "", base)
	hub.observe(few, liveDelta{phase: liveText, text: "abc"}, base)
	hub.observe(few, liveDelta{phase: liveText, text: "def"}, base.Add(400*time.Millisecond))
	if got := hub.snapshot("few", "", base.Add(500*time.Millisecond)).Speed; got != 0 {
		t.Fatalf("6-token window speed=%v, want 0", got)
	}

	capped := hub.begin("capped", "", "", base)
	hub.observe(capped, liveDelta{phase: liveText, text: strings.Repeat("x", 100_000)}, base)
	hub.observe(capped, liveDelta{phase: liveText, text: strings.Repeat("x", 100_000)}, base.Add(300*time.Millisecond))
	if got := hub.snapshot("capped", "", base.Add(400*time.Millisecond)).Speed; got != liveSpeedMax {
		t.Fatalf("capped speed=%v, want %v", got, liveSpeedMax)
	}
}

func TestLiveSessionTurnAndRetention(t *testing.T) {
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	var hub liveHub
	one := hub.begin("same", "turn-1", "m1", base)
	two := hub.begin("same", "turn-2", "m2", base.Add(time.Second))
	hub.observe(one, liveDelta{phase: liveReasoning, text: "one"}, base.Add(time.Millisecond))
	hub.observe(two, liveDelta{phase: liveTool, text: `{"path":"x"}`}, base.Add(time.Second+time.Millisecond))

	if got := hub.snapshot("same", "turn-1", base.Add(2*time.Second)); got.Turn != "turn-1" || got.Model != "m1" {
		t.Fatalf("turn-1 snapshot=%+v", got)
	}
	if got := hub.snapshot("same", "turn-2", base.Add(2*time.Second)); got.Turn != "turn-2" || got.Phase != string(liveTool) {
		t.Fatalf("turn-2 snapshot=%+v", got)
	}
	if got := hub.snapshot("same", "", base.Add(2*time.Second)); !got.Active || got.Turn != "turn-2" {
		t.Fatalf("latest active snapshot=%+v", got)
	}
	if got := hub.snapshot("other", "", base.Add(2*time.Second)); got.Active || got.Done {
		t.Fatalf("unrelated session returned %+v", got)
	}

	for i := 0; i < liveKept+5; i++ {
		run := hub.begin("bulk", "", "m", base.Add(time.Duration(i)*time.Millisecond))
		hub.finish(run, base.Add(time.Duration(i)*time.Millisecond+time.Microsecond))
	}
	if got := hub.snapshot("bulk", "", base.Add(3*time.Second)); !got.Done || got.Model != "m" {
		t.Fatalf("latest retained=%+v", got)
	}
	if got := hub.snapshot("same", "", base.Add(3*time.Second)); got.Active || got.Done {
		t.Fatalf("old run survived the 64-request cap: %+v", got)
	}

	var retention liveHub
	retained := retention.begin("expires", "turn", "m", base)
	retention.finish(retained, base.Add(time.Second))
	if got := retention.snapshot("expires", "turn", base.Add(30*time.Second)); !got.Done {
		t.Fatalf("done run expired before 30s: %+v", got)
	}
	if got := retention.snapshot("expires", "turn", base.Add(31*time.Second+time.Millisecond)); got.Active || got.Done {
		t.Fatalf("done run survived 30s: %+v", got)
	}
}

func TestLiveTokens(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"x", 1},
		{"abcd", 1},
		{"abcdefgh", 2},
		{"中文", 2},
		{"abc中文xyz", 3},
	} {
		if got := liveTokens(tc.text); got != tc.want {
			t.Errorf("liveTokens(%q)=%d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestLiveSSEDeltasFourProtocols(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		want  []liveDelta
	}{
		{
			name:  "responses",
			event: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n",
			want:  []liveDelta{{phase: liveText, text: "hello"}},
		},
		{
			name:  "chat",
			event: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\",\"content\":\"hello\",\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n",
			want: []liveDelta{
				{phase: liveReasoning, text: "think"},
				{phase: liveText, text: "hello"},
				{phase: liveTool, text: `{"path":`},
			},
		},
		{
			name:  "anthropic",
			event: "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"partial_json\",\"partial_json\":\"{\\\"query\\\":\"}}\n\n",
			want:  []liveDelta{{phase: liveTool, text: `{"query":`}},
		},
		{
			name:  "gemini",
			event: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"plan\",\"thought\":true},{\"text\":\"answer\"},{\"functionCall\":{\"name\":\"read\",\"args\":{\"path\":\"README.md\"}}}]}}]}\n\n",
			want: []liveDelta{
				{phase: liveReasoning, text: "plan"},
				{phase: liveText, text: "answer"},
				{phase: liveTool, text: `{"name":"read","args":{"path":"README.md"}}`},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := timingInfoOf([]byte(tc.event))
			if len(info.deltas) != len(tc.want) {
				t.Fatalf("deltas=%+v, want %+v", info.deltas, tc.want)
			}
			for i := range tc.want {
				if info.deltas[i] != tc.want[i] {
					t.Errorf("delta %d=%+v, want %+v", i, info.deltas[i], tc.want[i])
				}
			}
		})
	}
}

func TestLiveTrackFiltering(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"t\",\"thread_source\":\"user\"}"}}`)
	for _, tc := range []struct {
		name   string
		header http.Header
		want   bool
	}{
		{name: "main", header: http.Header{"Session-Id": {"s"}, "User-Agent": {"codex_cli_rs/1"}}, want: true},
		{name: "title", header: http.Header{"Session-Id": {"s"}, "User-Agent": {"codex_cli_rs/1"}, "X-Openai-Subagent": {"thread_title"}}},
		{name: "description", header: http.Header{"Session-Id": {"s"}, "User-Agent": {"codex_cli_rs/1"}, "X-Openai-Subagent": {"thread_description"}}},
		{name: "prediction", header: http.Header{"Session-Id": {"s"}, "User-Agent": {"codex_cli_rs/1"}, "X-Openai-Subagent": {"composer_predictions"}}},
		{name: "auxiliary", header: http.Header{"Session-Id": {"s"}, "User-Agent": {"codex_cli_rs/1"}, "X-Openai-Memgen-Request": {"1"}}},
		{name: "search", header: http.Header{"Session-Id": {"s"}, "User-Agent": {SearchAgent}}},
		{name: "vision", header: http.Header{"Session-Id": {"s"}, "User-Agent": {VisionAgent}}},
		{name: "subagent", header: http.Header{"Session-Id": {"s"}, "User-Agent": {"codex_cli_rs/1"}, "X-Openai-Subagent": {"collab_spawn"}}},
		{name: "no session", header: http.Header{"User-Agent": {"codex_cli_rs/1"}}},
		{name: "not codex", header: http.Header{"Session-Id": {"s"}, "User-Agent": {"claude-cli/1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
			req.Header = tc.header
			if tc.name != "not codex" {
				req.Header.Set("Authorization", "Bearer "+Token+"-codex")
			}
			metadata := requestSessionMetadata(req.Header, body)
			kind := requestCallKind(req.Header, metadata)
			if got := liveTracks(req, body, metadata, kind); got != tc.want {
				t.Fatalf("liveTracks=%v, want %v (agent=%q session=%q stream=%v kind=%q source=%q)",
					got, tc.want, agentOf(req), sessionOf(req.Header), streamOf(body), kind, metadata.Source)
			}
		})
	}
}

func TestLiveEndpointLoopbackAndShape(t *testing.T) {
	server := New()
	base := time.Now().Add(-2 * time.Second)
	server.liveState.begin("loop", "turn", "model", base)

	remote := httptest.NewRequest(http.MethodGet, "/v1/magpie/live?session=loop&turn=turn", nil)
	remote.RemoteAddr = "192.168.1.9:5000"
	remoteResult := httptest.NewRecorder()
	server.live(remoteResult, remote)
	if remoteResult.Code != http.StatusForbidden {
		t.Fatalf("remote status=%d body=%s", remoteResult.Code, remoteResult.Body.String())
	}

	proxied := httptest.NewRequest(http.MethodGet, "/v1/magpie/live?session=loop&turn=turn", nil)
	proxied.RemoteAddr = "127.0.0.1:5000"
	proxied.Header.Set("X-Forwarded-For", "203.0.113.9")
	proxiedResult := httptest.NewRecorder()
	server.live(proxiedResult, proxied)
	if proxiedResult.Code != http.StatusForbidden {
		t.Fatalf("proxied status=%d body=%s", proxiedResult.Code, proxiedResult.Body.String())
	}

	local := httptest.NewRequest(http.MethodGet, "/v1/magpie/live?session=loop&turn=turn", nil)
	local.RemoteAddr = "127.0.0.1:5000"
	localResult := httptest.NewRecorder()
	server.live(localResult, local)
	if localResult.Code != http.StatusOK || localResult.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("local status=%d headers=%v", localResult.Code, localResult.Header())
	}
	var got map[string]any
	if err := json.Unmarshal(localResult.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"active", "done", "session", "turn", "model", "phase", "started_at", "first_at",
		"updated_at", "estimated_tokens", "speed", "window_ms", "speed_estimated", "stale",
	} {
		if _, ok := got[field]; !ok {
			t.Errorf("missing %q in %v", field, got)
		}
	}
	if got["stale"] != true || got["active"] != true || got["speed"].(float64) != 0 {
		t.Fatalf("stale snapshot=%v", got)
	}
}

func TestLiveCodexUpstreamIntegration(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`event: response.created`+"\n"+`data: {"type":"response.created","response":{"id":"r1","model":"gpt-5.5"}}`,
			`event: response.reasoning_summary_text.delta`+"\n"+`data: {"type":"response.reasoning_summary_text.delta","delta":"think"}`,
			`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"hello world"}`,
			`event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"r1","model":"gpt-5.5","status":"completed","usage":{"input_tokens":10,"output_tokens":5}}}`,
		))
	}))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })

	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hi","client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-native\",\"thread_source\":\"user\"}"}}`)
	req := httptest.NewRequest(http.MethodPost, CodexPath+"/responses", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+Token+"-codex")
	req.Header.Set("User-Agent", "codex_cli_rs/1")
	req.Header.Set("Session-Id", "session-native")
	rec := httptest.NewRecorder()
	server := New()
	server.codexUpstreamOn(rec, req, "/responses", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	got := server.liveState.snapshot("session-native", "turn-native", time.Now())
	if !got.Done || got.Active || got.Model != "gpt-5.5" || got.Phase != string(liveText) {
		t.Fatalf("native live snapshot=%+v", got)
	}
	if got.EstimatedTokens < 3 {
		t.Fatalf("native estimated tokens=%d", got.EstimatedTokens)
	}
}

func TestLiveTranslatedIntegration(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"m1","choices":[{"delta":{"reasoning_content":"think","content":"hello world"}}]}`,
		`data: {"id":"c1","model":"m1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
	)}
	setup(t, provider.Chat, f)

	body := []byte(`{"model":"fake/m1","stream":true,"input":"hi","client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"turn-translated\",\"thread_source\":\"user\"}"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+Token+"-codex")
	req.Header.Set("User-Agent", "codex_cli_rs/1")
	req.Header.Set("Session-Id", "session-translated")
	rec := httptest.NewRecorder()
	server := New()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	got := server.liveState.snapshot("session-translated", "turn-translated", time.Now())
	if !got.Done || got.Active || got.Phase != string(liveText) {
		t.Fatalf("translated live snapshot=%+v", got)
	}
	if got.EstimatedTokens < 3 {
		t.Fatalf("translated estimated tokens=%d", got.EstimatedTokens)
	}
}
