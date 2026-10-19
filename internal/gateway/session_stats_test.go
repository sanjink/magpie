package gateway

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/usage"
)

func TestBuildSessionStatsCompletedOnlyAndExactSession(t *testing.T) {
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	rows := []usage.Row{
		{Record: usage.Record{
			Time: base, Agent: "codex", Provider: "test", Model: "m1", Session: "s-a",
			Input: 1000, Output: 100, CacheRead: 100, CacheWrite: 10,
			Millis: 2000, TTFT: 1000, Status: 200, ResponseID: "r1",
		}},
		{Record: usage.Record{
			Time: base.Add(3 * time.Second), Agent: "codex", Provider: "test", Model: "m2", Session: "s-a",
			Input: 2000, Output: 90, CacheRead: 200,
			Millis: 3000, TTFT: 1000, Status: 200, ResponseID: "r2",
		}},
		{Record: usage.Record{
			Time: base.Add(8 * time.Second), Agent: "codex", Provider: "test", Model: "m3", Session: "s-a",
			Millis: 500, Status: 500, Error: "failed", ResponseID: "r3",
		}},
		{Record: usage.Record{
			Time: base.Add(time.Second), Agent: "codex", Provider: "test", Model: "other", Session: "s-a-extra",
			Output: 9000, Millis: 2000, TTFT: 1000, Status: 200,
		}},
	}
	tools := []sessionToolSample{
		{start: base.Add(2500 * time.Millisecond), end: base.Add(4500 * time.Millisecond)},
	}

	got := buildSessionStats("s-a", rows, tools)
	if got.ModelMS != 5500 || got.ToolMS != 2000 || got.Steps != 3 {
		t.Fatalf("durations/steps: %+v", got)
	}
	if got.TTFTCount != 2 || got.TTFTMS == nil || *got.TTFTMS != 1000 {
		t.Fatalf("ttft: %+v", got)
	}
	if got.DecodeTokens != 190 || got.DecodeMS != 3000 ||
		got.TPS == nil || math.Abs(*got.TPS-63.333333) > 0.001 {
		t.Fatalf("weighted tps: %+v", got)
	}
	if got.PeakTPS == nil || *got.PeakTPS != 100 {
		t.Fatalf("peak: %+v", got)
	}
	if got.Tokens != (SessionTokenStats{Input: 3000, Output: 190, CacheRead: 300, CacheWrite: 10}) {
		t.Fatalf("tokens: %+v", got.Tokens)
	}
	if got.Cache != (SessionCacheStats{Read: 300, Write: 10}) {
		t.Fatalf("cache: %+v", got.Cache)
	}
	if got.ActiveMS != 6000 {
		t.Fatalf("active=%d, want 6000", got.ActiveMS)
	}
	if len(got.Series) != 2 || got.Series[0].CallID != "r1" || got.Series[1].CallID != "r2" {
		t.Fatalf("series: %+v", got.Series)
	}

	live := liveSnapshot{Active: true, Speed: 999, Model: "live", UpdatedAt: &base}
	withLive := withSessionProvisional(got, live, base.Add(time.Minute))
	if withLive.Provisional == nil || withLive.Provisional.TPS != 999 {
		t.Fatalf("provisional: %+v", withLive.Provisional)
	}
	if withLive.PeakTPS == nil || *withLive.PeakTPS != 100 || withLive.TPS == nil || *withLive.TPS != *got.TPS {
		t.Fatalf("provisional changed completed stats: %+v", withLive)
	}
}

func TestSessionStatsEndpointLoopbackAndShape(t *testing.T) {
	fresh(t)
	now := time.Now().Add(-3 * time.Second)
	usage.Append(usage.Record{
		Time: now, Agent: "codex", Provider: "test", Model: "m1", Session: "stats-session",
		Input: 1000, Output: 100, CacheRead: 400,
		Millis: 2000, TTFT: 1000, Status: 200, ResponseID: "stats-r1",
	})
	usage.Append(usage.Record{
		Time: now.Add(time.Second), Agent: "codex", Provider: "test", Model: "other", Session: "other-session",
		Input: 9999, Output: 9999, Millis: 2000, TTFT: 1000, Status: 200,
	})
	server := New()

	local := httptest.NewRequest(http.MethodGet, "/v1/magpie/session/stats?session=stats-session", nil)
	local.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	server.sessionStats(rec, local)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var got SessionStats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Session != "stats-session" || got.Steps != 1 || got.Tokens.Input != 1000 ||
		got.Tokens.Output != 100 || got.PeakTPS == nil || *got.PeakTPS != 100 {
		t.Fatalf("stats=%+v", got)
	}
	if got.Provisional != nil {
		t.Fatalf("an already completed request appeared provisional: %+v", got.Provisional)
	}

	remote := httptest.NewRequest(http.MethodGet, "/v1/magpie/session/stats?session=stats-session", nil)
	remote.RemoteAddr = "192.168.1.9:5000"
	remoteRec := httptest.NewRecorder()
	server.sessionStats(remoteRec, remote)
	if remoteRec.Code != http.StatusForbidden {
		t.Fatalf("remote status=%d body=%s", remoteRec.Code, remoteRec.Body.String())
	}

	missing := httptest.NewRequest(http.MethodGet, "/v1/magpie/session/stats", nil)
	missing.RemoteAddr = "127.0.0.1:5000"
	missingRec := httptest.NewRecorder()
	server.sessionStats(missingRec, missing)
	if missingRec.Code != http.StatusBadRequest {
		t.Fatalf("missing session status=%d", missingRec.Code)
	}
}
