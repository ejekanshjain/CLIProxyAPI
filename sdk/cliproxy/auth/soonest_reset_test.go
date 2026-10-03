package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSoonestResetPicksSoonerWeeklyReset(t *testing.T) {
	t.Parallel()

	sooner, later := resetPair()
	auths := []*Auth{
		claudeAuth("a-later", later, time.Time{}, "allowed", "allowed"),
		claudeAuth("z-sooner", sooner, time.Time{}, "allowed", "allowed"),
	}
	assertSoonestPick(t, "claude", auths, "z-sooner")
}

func TestSoonestResetFollowsMovedWeeklyReset(t *testing.T) {
	t.Parallel()

	sooner, later := resetPair()
	moved := later.Add(24 * time.Hour)

	t.Run("selector", func(t *testing.T) {
		t.Parallel()
		selector := &SoonestResetSelector{}
		soonerAuth := claudeAuth("z-sooner", sooner, time.Time{}, "allowed", "allowed")
		laterAuth := claudeAuth("a-later", later, time.Time{}, "allowed", "allowed")
		auths := []*Auth{laterAuth, soonerAuth}
		if got := pickSelector(t, selector, "claude", auths); got != "z-sooner" {
			t.Fatalf("first pick = %s, want z-sooner", got)
		}
		soonerAuth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = unixSignal(moved)
		if got := pickSelector(t, selector, "claude", auths); got != "a-later" {
			t.Fatalf("pick after moved reset = %s, want a-later", got)
		}
	})

	t.Run("request", func(t *testing.T) {
		t.Parallel()
		manager := newSoonestManager(t, &SoonestResetSelector{}, "claude")
		registerAuth(t, manager, claudeAuth("a-later", later, time.Time{}, "allowed", "allowed"))
		registerAuth(t, manager, claudeAuth("z-sooner", sooner, time.Time{}, "allowed", "allowed"))
		if got := pickRequest(t, manager, "claude", ""); got != "z-sooner" {
			t.Fatalf("first request = %s, want z-sooner", got)
		}

		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{
			"Anthropic-Ratelimit-Unified-7d-Reset":  []string{unixSignal(moved)},
			"Anthropic-Ratelimit-Unified-7d-Status": []string{"allowed"},
			"Anthropic-Ratelimit-Unified-5h-Status": []string{"allowed"},
		})
		manager.MarkResult(ctx, Result{
			AuthID:   "z-sooner",
			Provider: "claude",
			Success:  true,
		})
		if got := pickRequest(t, manager, "claude", "session-after-move"); got != "a-later" {
			t.Fatalf("request after moved reset = %s, want a-later", got)
		}
	})
}

func TestSoonestResetSkipsFiveHourCap(t *testing.T) {
	t.Parallel()

	soonest, middle, latest := resetTrio()
	fiveHourEnd := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	auths := []*Auth{
		claudeAuth("a-latest", latest, time.Time{}, "allowed", "allowed"),
		claudeAuth("m-room", middle, time.Time{}, "allowed", "allowed"),
		claudeAuth("z-capped", soonest, fiveHourEnd, "allowed", "rejected"),
	}
	assertSoonestPick(t, "claude", auths, "m-room")
}

func TestSoonestResetUsesSoonestFiveHourResetWhenEveryAccountIsCapped(t *testing.T) {
	t.Parallel()

	soonerWeekly, laterWeekly := resetPair()
	soonerFiveHour := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	laterFiveHour := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	auths := []*Auth{
		claudeAuth("z-weekly-sooner", soonerWeekly, laterFiveHour, "allowed", "rejected"),
		claudeAuth("a-five-hour-sooner", laterWeekly, soonerFiveHour, "allowed", "rejected"),
	}
	assertSoonestPick(t, "claude", auths, "a-five-hour-sooner")
}

func TestSoonestResetSessionAffinityStaysThenFailsOverOnce(t *testing.T) {
	t.Parallel()

	sooner, later := resetPair()
	selector := NewSessionAffinitySelector(&SoonestResetSelector{})
	t.Cleanup(selector.Stop)
	manager := newSoonestManager(t, selector, "claude")
	registerAuth(t, manager, claudeAuth("a-later", later, time.Time{}, "allowed", "allowed"))
	registerAuth(t, manager, claudeAuth("z-sooner", sooner, time.Time{}, "allowed", "allowed"))

	if got := pickRequest(t, manager, "claude", "thread-healthy"); got != "z-sooner" {
		t.Fatalf("first request = %s, want z-sooner", got)
	}
	if got := pickRequest(t, manager, "claude", "thread-healthy"); got != "z-sooner" {
		t.Fatalf("second request = %s, want z-sooner", got)
	}

	coolAuth(t, manager, "z-sooner")
	if got := pickRequest(t, manager, "claude", "thread-healthy"); got != "a-later" {
		t.Fatalf("request after cooling = %s, want a-later", got)
	}
	if got := pickRequest(t, manager, "claude", "thread-healthy"); got != "a-later" {
		t.Fatalf("request after failover = %s, want a-later", got)
	}

	restoreAuth(t, manager, "z-sooner")
	if got := pickRequest(t, manager, "claude", "thread-healthy"); got != "a-later" {
		t.Fatalf("request after the first account recovered = %s, want a-later", got)
	}
}

func TestRoundRobinAndFillFirstIgnoreWeeklyReset(t *testing.T) {
	t.Parallel()

	sooner, later := resetPair()
	auths := []*Auth{
		claudeAuth("a-later", later, time.Time{}, "allowed", "allowed"),
		claudeAuth("z-sooner", sooner, time.Time{}, "allowed", "allowed"),
	}

	t.Run("round-robin", func(t *testing.T) {
		t.Parallel()
		manager := newSoonestManager(t, &RoundRobinSelector{}, "claude")
		for _, auth := range auths {
			registerAuth(t, manager, auth.Clone())
		}
		if got := pickRequest(t, manager, "claude", ""); got != "a-later" {
			t.Fatalf("first round-robin pick = %s, want a-later", got)
		}
		if got := pickRequest(t, manager, "claude", ""); got != "z-sooner" {
			t.Fatalf("second round-robin pick = %s, want z-sooner", got)
		}
		if got := pickRequest(t, manager, "claude", ""); got != "a-later" {
			t.Fatalf("third round-robin pick = %s, want a-later", got)
		}
	})

	t.Run("fill-first", func(t *testing.T) {
		t.Parallel()
		manager := newSoonestManager(t, &FillFirstSelector{}, "claude")
		for _, auth := range auths {
			registerAuth(t, manager, auth.Clone())
		}
		for index := 0; index < 3; index++ {
			if got := pickRequest(t, manager, "claude", ""); got != "a-later" {
				t.Fatalf("fill-first pick %d = %s, want a-later", index, got)
			}
		}
	})
}

func TestSoonestResetKeepsProviderPoolsSeparate(t *testing.T) {
	t.Parallel()

	sooner, later := resetPair()
	claudeAccount := claudeAuth("claude-later", later, time.Time{}, "allowed", "allowed")
	codexAccount := &Auth{
		ID:       "codex-sooner",
		Provider: "codex",
		Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes":   "10080",
			"X-Codex-Primary-Reset-At":         unixSignal(sooner),
			"X-Codex-Primary-Used-Percent":     "10",
			"X-Codex-Secondary-Window-Minutes": "300",
			"X-Codex-Secondary-Reset-At":       unixSignal(sooner.Add(time.Hour)),
			"X-Codex-Secondary-Used-Percent":   "10",
		}},
	}
	assertSoonestPick(t, "claude", []*Auth{claudeAccount, codexAccount}, "claude-later")

	claudeSooner := claudeAuth("claude-sooner", sooner, time.Time{}, "allowed", "allowed")
	codexLater := &Auth{
		ID:       "codex-later",
		Provider: "codex",
		Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes":   "10080",
			"X-Codex-Primary-Reset-At":         unixSignal(later),
			"X-Codex-Primary-Used-Percent":     "10",
			"X-Codex-Secondary-Window-Minutes": "300",
			"X-Codex-Secondary-Reset-At":       unixSignal(later.Add(time.Hour)),
			"X-Codex-Secondary-Used-Percent":   "10",
		}},
	}
	assertSoonestPick(t, "codex", []*Auth{claudeSooner, codexLater}, "codex-later")
}

func TestSoonestResetUsesStoredCodexWindowLength(t *testing.T) {
	t.Parallel()

	fiveHourReset := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	soonerWeekly := fiveHourReset.Add(24 * time.Hour)
	laterWeekly := soonerWeekly.Add(48 * time.Hour)
	secondaryWeekly := &Auth{
		ID:       "a-secondary-weekly",
		Provider: "codex",
		Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes":   "300",
			"X-Codex-Primary-Reset-At":         unixSignal(fiveHourReset),
			"X-Codex-Primary-Used-Percent":     "10",
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       unixSignal(laterWeekly),
			"X-Codex-Secondary-Used-Percent":   "10",
		}},
	}
	primaryWeekly := &Auth{
		ID:       "z-primary-weekly",
		Provider: "codex",
		Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes": "10080",
			"X-Codex-Primary-Reset-At":       unixSignal(soonerWeekly),
			"X-Codex-Primary-Used-Percent":   "10",
		}},
	}
	assertSoonestPick(t, "codex", []*Auth{secondaryWeekly, primaryWeekly}, "z-primary-weekly")
}

func TestSoonestResetRanksMissingWeeklyResetAfterKnownTimes(t *testing.T) {
	t.Parallel()

	_, later := resetPair()
	withReset := claudeAuth("z-known", later, time.Time{}, "allowed", "allowed")
	missingEarly := &Auth{ID: "a-missing", Provider: "claude"}
	missingLate := &Auth{ID: "m-missing", Provider: "claude"}
	assertSoonestPick(t, "claude", []*Auth{missingEarly, missingLate, withReset}, "z-known")
	assertSoonestPick(t, "claude", []*Auth{missingLate, missingEarly}, "a-missing")
}

func TestSoonestResetBreaksWeeklyTiesByAccountID(t *testing.T) {
	t.Parallel()

	reset, _ := resetPair()
	auths := []*Auth{
		claudeAuth("z-tie", reset, time.Time{}, "allowed", "allowed"),
		claudeAuth("a-tie", reset, time.Time{}, "allowed", "allowed"),
	}
	assertSoonestPick(t, "claude", auths, "a-tie")
}

func TestSoonestResetSkipsUnusableAccounts(t *testing.T) {
	t.Parallel()

	sooner, later := resetPair()
	healthy := claudeAuth("a-healthy", later, time.Time{}, "allowed", "allowed")

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		skipped := claudeAuth("z-disabled", sooner, time.Time{}, "allowed", "allowed")
		skipped.Disabled = true
		assertSoonestPick(t, "claude", []*Auth{skipped, healthy}, "a-healthy")
	})

	t.Run("cooling", func(t *testing.T) {
		t.Parallel()
		skipped := claudeAuth("z-cooling", sooner, time.Time{}, "allowed", "allowed")
		skipped.Unavailable = true
		skipped.NextRetryAfter = time.Now().Add(time.Hour)
		assertSoonestPick(t, "claude", []*Auth{skipped, healthy}, "a-healthy")
	})

	t.Run("unauthorized", func(t *testing.T) {
		t.Parallel()
		skipped := claudeAuth("z-unauthorized", sooner, time.Time{}, "allowed", "allowed")
		skipped.Unavailable = true
		skipped.Status = StatusError
		skipped.LastError = &Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
		assertSoonestPick(t, "claude", []*Auth{skipped, healthy}, "a-healthy")
	})

	t.Run("expired", func(t *testing.T) {
		t.Parallel()
		skipped := claudeAuth("z-expired", sooner, time.Time{}, "allowed", "allowed")
		skipped.Metadata = map[string]any{
			"access_token": "opaque-token",
			"expired":      time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		}
		assertSoonestPick(t, "claude", []*Auth{skipped, healthy}, "a-healthy")
	})

	t.Run("weekly quota", func(t *testing.T) {
		t.Parallel()
		skipped := claudeAuth("z-weekly", sooner, time.Time{}, "rejected", "allowed")
		assertSoonestPick(t, "claude", []*Auth{skipped, healthy}, "a-healthy")
	})
}

func TestSoonestResetReadsDevinWeeklyReset(t *testing.T) {
	t.Parallel()

	sooner, later := resetPair()
	auths := []*Auth{
		{
			ID:       "a-devin-later",
			Provider: "devin",
			Quota: QuotaState{Signals: map[string]string{
				"weekly_quota_reset_at":          later.Format(time.RFC3339),
				"weekly_quota_remaining_percent": "40%",
			}},
		},
		{
			ID:       "z-devin-sooner",
			Provider: "devin",
			Quota: QuotaState{Signals: map[string]string{
				"weekly_quota_reset_at":          sooner.Format(time.RFC3339),
				"weekly_quota_remaining_percent": "0%",
			}},
		},
		{
			ID:       "m-devin-room",
			Provider: "devin",
			Quota: QuotaState{Signals: map[string]string{
				"weekly_quota_reset_at":          later.Add(time.Hour).Format(time.RFC3339),
				"weekly_quota_remaining_percent": "15%",
			}},
		},
	}
	assertSoonestPick(t, "devin", auths, "a-devin-later")
}

func TestSoonestResetReopensWindowAfterStoredResetPasses(t *testing.T) {
	t.Parallel()

	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	future := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	t.Run("passed weekly reset ranks after a known future reset", func(t *testing.T) {
		t.Parallel()
		stale := claudeAuth("a-stale", past, time.Time{}, "allowed", "allowed")
		known := claudeAuth("z-future", future, time.Time{}, "allowed", "allowed")
		assertSoonestPick(t, "claude", []*Auth{stale, known}, "z-future")
	})

	t.Run("future weekly exhaustion stays closed", func(t *testing.T) {
		t.Parallel()
		stale := claudeAuth("z-stale", past, time.Time{}, "rejected", "allowed")
		closed := claudeAuth("a-closed", future, time.Time{}, "rejected", "allowed")
		assertSoonestPick(t, "claude", []*Auth{closed, stale}, "z-stale")
	})

	t.Run("passed 5-hour cap is open", func(t *testing.T) {
		t.Parallel()
		opened := claudeAuth("z-opened", future, past, "allowed", "rejected")
		later := claudeAuth("a-later", future.Add(7*24*time.Hour), time.Time{}, "allowed", "allowed")
		assertSoonestPick(t, "claude", []*Auth{later, opened}, "z-opened")
	})
}

func TestSoonestResetUsesStoredTimeOnDifferentWeekdays(t *testing.T) {
	t.Parallel()

	sunday := futureWeekday(time.Sunday)
	wednesday := futureWeekday(time.Wednesday)
	earlier, later := wednesday, sunday
	earlierName, laterName := "wednesday", "sunday"
	if sunday.Before(wednesday) {
		earlier, later = sunday, wednesday
		earlierName, laterName = "sunday", "wednesday"
	}

	t.Run("claude", func(t *testing.T) {
		t.Parallel()
		auths := []*Auth{
			claudeAuth("z-"+earlierName, earlier, time.Time{}, "allowed", "allowed"),
			claudeAuth("a-"+laterName, later, time.Time{}, "allowed", "allowed"),
		}
		assertSoonestPick(t, "claude", auths, "z-"+earlierName)
	})

	t.Run("codex", func(t *testing.T) {
		t.Parallel()
		laterAccount := &Auth{
			ID:       "a-" + laterName,
			Provider: "codex",
			Quota: QuotaState{Signals: map[string]string{
				"X-Codex-Primary-Window-Minutes": "10080",
				"X-Codex-Primary-Reset-At":       unixSignal(later),
				"X-Codex-Primary-Used-Percent":   "10",
			}},
		}
		earlierAccount := &Auth{
			ID:       "z-" + earlierName,
			Provider: "codex",
			Quota: QuotaState{Signals: map[string]string{
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       unixSignal(earlier),
				"X-Codex-Secondary-Used-Percent":   "10",
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Primary-Reset-At":         unixSignal(earlier.Add(time.Hour)),
				"X-Codex-Primary-Used-Percent":     "10",
			}},
		}
		assertSoonestPick(t, "codex", []*Auth{laterAccount, earlierAccount}, "z-"+earlierName)
	})
}

func futureWeekday(day time.Weekday) time.Time {
	now := time.Now().UTC()
	candidate := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	for candidate.Weekday() != day || !candidate.After(now) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}

func assertSoonestPick(t *testing.T, provider string, auths []*Auth, wantID string) {
	t.Helper()
	selectorAuths := cloneAuthList(auths)
	if got := pickSelector(t, &SoonestResetSelector{}, provider, selectorAuths); got != wantID {
		t.Fatalf("selector pick = %s, want %s", got, wantID)
	}

	providers := map[string]struct{}{provider: {}}
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		authProvider := auth.Provider
		if authProvider == "" {
			authProvider = provider
		}
		providers[authProvider] = struct{}{}
	}
	for authProvider := range providers {
		manager.executors[authProvider] = schedulerTestExecutor{provider: authProvider}
	}
	for _, auth := range auths {
		registerAuth(t, manager, auth.Clone())
	}
	if got := pickRequest(t, manager, provider, ""); got != wantID {
		t.Fatalf("request pick = %s, want %s", got, wantID)
	}
}

func pickSelector(t *testing.T, selector Selector, provider string, auths []*Auth) string {
	t.Helper()
	picked, errPick := selector.Pick(context.Background(), provider, "", cliproxyexecutor.Options{}, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if picked == nil {
		t.Fatal("Pick() returned no account")
	}
	return picked.ID
}

func newSoonestManager(t *testing.T, selector Selector, providers ...string) *Manager {
	t.Helper()
	manager := NewManager(nil, selector, nil)
	for _, provider := range providers {
		manager.executors[provider] = schedulerTestExecutor{provider: provider}
	}
	return manager
}

func registerAuth(t *testing.T, manager *Manager, auth *Auth) {
	t.Helper()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
	}
}

func pickRequest(t *testing.T, manager *Manager, provider, sessionID string) string {
	t.Helper()
	opts := cliproxyexecutor.Options{}
	if sessionID != "" {
		opts.OriginalRequest = []byte(`{"session_id":"` + sessionID + `"}`)
	}
	picked, _, errPick := manager.pickNext(context.Background(), provider, "", opts, nil)
	if errPick != nil {
		t.Fatalf("pickNext(%s) error = %v", provider, errPick)
	}
	if picked == nil {
		t.Fatal("pickNext() returned no account")
	}
	return picked.ID
}

func coolAuth(t *testing.T, manager *Manager, authID string) {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("account %s was not found", authID)
	}
	auth.Unavailable = true
	auth.Status = StatusError
	auth.NextRetryAfter = time.Now().Add(time.Hour)
	if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("Update(%s) error = %v", authID, errUpdate)
	}
}

func restoreAuth(t *testing.T, manager *Manager, authID string) {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("account %s was not found", authID)
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.NextRetryAfter = time.Time{}
	if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("Update(%s) error = %v", authID, errUpdate)
	}
}

func claudeAuth(id string, weeklyReset, fiveHourReset time.Time, weeklyStatus, fiveHourStatus string) *Auth {
	signals := make(map[string]string)
	if !weeklyReset.IsZero() {
		signals["Anthropic-Ratelimit-Unified-7d-Reset"] = unixSignal(weeklyReset)
	}
	if weeklyStatus != "" {
		signals["Anthropic-Ratelimit-Unified-7d-Status"] = weeklyStatus
	}
	if !fiveHourReset.IsZero() {
		signals["Anthropic-Ratelimit-Unified-5h-Reset"] = unixSignal(fiveHourReset)
	}
	if fiveHourStatus != "" {
		signals["Anthropic-Ratelimit-Unified-5h-Status"] = fiveHourStatus
	}
	return &Auth{
		ID:       id,
		Provider: "claude",
		Quota:    QuotaState{Signals: signals},
	}
}

func cloneAuthList(auths []*Auth) []*Auth {
	cloned := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			cloned = append(cloned, nil)
			continue
		}
		cloned = append(cloned, auth.Clone())
	}
	return cloned
}

func resetPair() (time.Time, time.Time) {
	sooner := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	return sooner, sooner.Add(7 * 24 * time.Hour)
}

func resetTrio() (time.Time, time.Time, time.Time) {
	soonest, middle := resetPair()
	return soonest, middle, middle.Add(7 * 24 * time.Hour)
}

func unixSignal(ts time.Time) string {
	return strconv.FormatInt(ts.Unix(), 10)
}
