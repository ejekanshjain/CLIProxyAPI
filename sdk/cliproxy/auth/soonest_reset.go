package auth

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	fiveHourWindowMinutes = 300
	weeklyWindowMinutes   = 7 * 24 * 60
)

// SoonestResetSelector picks the ready account whose stored weekly reset comes soonest.
// Every choice reads the stored time again. The selector does not assume a weekday.
// A request for one provider uses only that provider's accounts.
// An account inside its 5-hour cap is skipped while another account still has 5-hour room.
// When every ready account is inside that cap, the soonest 5-hour reset wins.
// An account with no weekly reset time ranks after accounts that have one, then by account id.
type SoonestResetSelector struct{}

// Pick selects one ready account with the soonest-reset order.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	pooled := authsForProviderPool(provider, auths)
	available, errAvailable := getSelectorAvailableAuths(ctx, pooled, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	preferred := preferCodexWebsocketAuths(ctx, provider, available)
	picked := pickSoonestResetAuth(preferred, model, now)
	if picked == nil && len(preferred) < len(available) {
		picked = pickSoonestResetAuth(available, model, now)
	}
	if picked == nil {
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}
	return picked, nil
}

// authsForProviderPool keeps accounts that belong to the requested provider.
// Accounts with an empty provider stay eligible so callers that already filtered
// the pool do not lose them. A mixed request keeps every supplied account.
func authsForProviderPool(provider string, auths []*Auth) []*Auth {
	providerKey := canonicalSchedulingProvider(provider)
	if providerKey == "" || providerKey == "mixed" {
		return auths
	}
	pooled := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		authKey := canonicalSchedulingProvider(auth.Provider)
		if authKey == "" || authKey == providerKey {
			pooled = append(pooled, auth)
		}
	}
	return pooled
}

type quotaWindow struct {
	reset     time.Time
	hasReset  bool
	exhausted bool
	known     bool
}

type accountQuotaPlan struct {
	weeklyReset      time.Time
	hasWeeklyReset   bool
	weeklyExhausted  bool
	fiveHourReset    time.Time
	hasFiveHourReset bool
	fiveHourCapped   bool
}

type soonestResetCandidate struct {
	auth *Auth
	plan accountQuotaPlan
}

// pickSoonestResetAuth orders ready accounts by the weekly reset stored on each account.
// It skips accounts that are disabled, cooling, unauthorized, expired, or out of weekly quota.
func pickSoonestResetAuth(auths []*Auth, model string, now time.Time) *Auth {
	if len(auths) == 0 {
		return nil
	}
	ready := make([]soonestResetCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		blocked, _, _ := isAuthBlockedForModel(auth, model, now)
		if blocked {
			continue
		}
		plan := readAccountQuotaPlan(auth, now)
		if plan.weeklyExhausted {
			continue
		}
		ready = append(ready, soonestResetCandidate{auth: auth, plan: plan})
	}
	if len(ready) == 0 {
		return nil
	}
	sort.Slice(ready, func(i, j int) bool {
		return weeklyResetBefore(ready[i], ready[j])
	})
	for _, candidate := range ready {
		if !candidate.plan.fiveHourCapped {
			return candidate.auth
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		return fiveHourResetBefore(ready[i], ready[j])
	})
	return ready[0].auth
}

func weeklyResetBefore(left, right soonestResetCandidate) bool {
	if left.plan.hasWeeklyReset != right.plan.hasWeeklyReset {
		return left.plan.hasWeeklyReset
	}
	if left.plan.hasWeeklyReset && !left.plan.weeklyReset.Equal(right.plan.weeklyReset) {
		return left.plan.weeklyReset.Before(right.plan.weeklyReset)
	}
	return left.auth.ID < right.auth.ID
}

func fiveHourResetBefore(left, right soonestResetCandidate) bool {
	if left.plan.hasFiveHourReset != right.plan.hasFiveHourReset {
		return left.plan.hasFiveHourReset
	}
	if left.plan.hasFiveHourReset && !left.plan.fiveHourReset.Equal(right.plan.fiveHourReset) {
		return left.plan.fiveHourReset.Before(right.plan.fiveHourReset)
	}
	return left.auth.ID < right.auth.ID
}

// readAccountQuotaPlan reads the account snapshot stored in quota signals.
// Claude, Codex, and Devin keep that snapshot in different fields. The reset
// time is whatever timestamp is stored. Nothing here derives a weekday.
func readAccountQuotaPlan(auth *Auth, now time.Time) accountQuotaPlan {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return accountQuotaPlan{}
	}
	weekly, fiveHour := readProviderWindows(canonicalSchedulingProvider(auth.Provider), auth.Quota.Signals, auth.Quota.ObservedAt)
	weekly = elapseStoredReset(weekly, now)
	fiveHour = elapseStoredReset(fiveHour, now)
	return accountQuotaPlan{
		weeklyReset:      weekly.reset,
		hasWeeklyReset:   weekly.hasReset,
		weeklyExhausted:  weekly.exhausted,
		fiveHourReset:    fiveHour.reset,
		hasFiveHourReset: fiveHour.hasReset,
		fiveHourCapped:   fiveHour.exhausted,
	}
}

// elapseStoredReset drops a window whose stored reset is already due.
// The old usage belongs to the window that ended. The old timestamp is not
// the next reset, and the provider must store that next time.
func elapseStoredReset(window quotaWindow, now time.Time) quotaWindow {
	if !window.hasReset || now.IsZero() || window.reset.After(now) {
		return window
	}
	window.exhausted = false
	window.hasReset = false
	window.reset = time.Time{}
	return window
}

func readProviderWindows(provider string, signals map[string]string, observedAt time.Time) (quotaWindow, quotaWindow) {
	switch provider {
	case "claude", "anthropic":
		return readClaudeWindows(signals)
	case "codex":
		return readCodexWindows(signals, observedAt)
	case "devin":
		return readDevinWeekly(signals), quotaWindow{}
	default:
		weekly, fiveHour := readClaudeWindows(signals)
		if weekly.known || fiveHour.known {
			return weekly, fiveHour
		}
		weekly, fiveHour = readCodexWindows(signals, observedAt)
		if weekly.known || fiveHour.known {
			return weekly, fiveHour
		}
		return readDevinWeekly(signals), quotaWindow{}
	}
}

func readClaudeWindows(signals map[string]string) (quotaWindow, quotaWindow) {
	weekly := readNamedWindow(signals,
		"Anthropic-Ratelimit-Unified-7d-Reset",
		"Anthropic-Ratelimit-Unified-7d-Status",
		"Anthropic-Ratelimit-Unified-7d-Utilization",
		false, false)
	fiveHour := readNamedWindow(signals,
		"Anthropic-Ratelimit-Unified-5h-Reset",
		"Anthropic-Ratelimit-Unified-5h-Status",
		"Anthropic-Ratelimit-Unified-5h-Utilization",
		false, false)
	return weekly, fiveHour
}

func readDevinWeekly(signals map[string]string) quotaWindow {
	return readNamedWindow(signals, "weekly_quota_reset_at", "", "weekly_quota_remaining_percent", true, true)
}

func readNamedWindow(signals map[string]string, resetKey, statusKey, usageKey string, usageIsPercent, usageIsRemaining bool) quotaWindow {
	resetRaw := signalValue(signals, resetKey)
	statusRaw := signalValue(signals, statusKey)
	usageRaw := signalValue(signals, usageKey)
	if resetRaw == "" && statusRaw == "" && usageRaw == "" {
		return quotaWindow{}
	}
	window := quotaWindow{
		known:     true,
		exhausted: windowExhausted(statusRaw, usageRaw, usageIsPercent, usageIsRemaining),
	}
	if reset, ok := parseStoredTime(resetRaw); ok {
		window.reset = reset
		window.hasReset = true
	}
	return window
}

// readCodexWindows classifies primary and secondary by the stored window length.
// Codex does not promise that primary is the weekly window.
func readCodexWindows(signals map[string]string, observedAt time.Time) (quotaWindow, quotaWindow) {
	var weekly quotaWindow
	var fiveHour quotaWindow
	for _, name := range []string{"primary", "secondary"} {
		prefix := "x-codex-" + name + "-"
		minutesRaw := signalValue(signals, prefix+"window-minutes")
		resetAt := signalValue(signals, prefix+"reset-at")
		resetAfter := signalValue(signals, prefix+"reset-after-seconds")
		used := signalValue(signals, prefix+"used-percent")
		if minutesRaw == "" && resetAt == "" && resetAfter == "" && used == "" {
			continue
		}
		weeklyWindow, fiveHourWindow := classifyStoredWindowMinutes(parseWindowMinutes(minutesRaw))
		if !weeklyWindow && !fiveHourWindow {
			continue
		}
		window := quotaWindow{
			known:     true,
			exhausted: windowExhausted("", used, true, false),
		}
		if reset, ok := parseCodexReset(resetAt, resetAfter, observedAt); ok {
			window.reset = reset
			window.hasReset = true
		}
		if weeklyWindow {
			weekly = mergeQuotaWindow(weekly, window)
		}
		if fiveHourWindow {
			fiveHour = mergeQuotaWindow(fiveHour, window)
		}
	}
	return weekly, fiveHour
}

func mergeQuotaWindow(current, next quotaWindow) quotaWindow {
	if !current.known {
		return next
	}
	if next.exhausted {
		current.exhausted = true
	}
	if next.hasReset && (!current.hasReset || next.reset.Before(current.reset)) {
		current.reset = next.reset
		current.hasReset = true
	}
	current.known = true
	return current
}

// classifyStoredWindowMinutes maps a stored duration to the weekly or 5-hour window.
// Durations outside those bands are ignored instead of being treated as a weekday.
func classifyStoredWindowMinutes(minutes int) (weekly bool, fiveHour bool) {
	if minutes >= fiveHourWindowMinutes-30 && minutes <= fiveHourWindowMinutes+30 {
		return false, true
	}
	if minutes >= weeklyWindowMinutes-12*60 && minutes <= weeklyWindowMinutes+12*60 {
		return true, false
	}
	return false, false
}

func parseWindowMinutes(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || value <= 0 {
		return 0
	}
	return int(value)
}

func parseCodexReset(resetAt, resetAfter string, observedAt time.Time) (time.Time, bool) {
	if reset, ok := parseStoredTime(resetAt); ok {
		return reset, true
	}
	seconds, errParse := strconv.ParseInt(strings.TrimSpace(resetAfter), 10, 64)
	if errParse != nil || seconds < 0 || observedAt.IsZero() {
		return time.Time{}, false
	}
	return observedAt.Add(time.Duration(seconds) * time.Second), true
}

func parseStoredTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
		return parsed, true
	}
	seconds, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || seconds <= 0 {
		return time.Time{}, false
	}
	if seconds > 1_000_000_000_000 {
		return time.UnixMilli(int64(seconds)).UTC(), true
	}
	whole := int64(seconds)
	fraction := seconds - float64(whole)
	return time.Unix(whole, int64(fraction*float64(time.Second))).UTC(), true
}

func windowExhausted(status, usage string, usageIsPercent, usageIsRemaining bool) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "rejected", "limited", "exceeded", "denied":
		return true
	case "allowed", "ok", "accepted":
		return false
	}
	value, percent, ok := parseUsageNumber(usage)
	if !ok {
		return false
	}
	if usageIsRemaining {
		return value <= 0
	}
	if percent || usageIsPercent || value > 1 {
		return value >= 100
	}
	return value >= 1
}

func parseUsageNumber(raw string) (float64, bool, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, false
	}
	percent := strings.HasSuffix(raw, "%")
	if percent {
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil {
		return 0, false, false
	}
	return value, percent, true
}

func signalValue(signals map[string]string, key string) string {
	if len(signals) == 0 || strings.TrimSpace(key) == "" {
		return ""
	}
	if value, ok := signals[key]; ok {
		return strings.TrimSpace(value)
	}
	if canonical := http.CanonicalHeaderKey(key); canonical != key {
		if value, ok := signals[canonical]; ok {
			return strings.TrimSpace(value)
		}
	}
	for name, value := range signals {
		if strings.EqualFold(name, key) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// pickSoonestReset selects one ready scheduler entry with the soonest-reset order.
func (v *readyView) pickSoonestReset(predicate func(*scheduledAuth) bool, model string, now time.Time) *scheduledAuth {
	if v == nil || len(v.flat) == 0 {
		return nil
	}
	auths := make([]*Auth, 0, len(v.flat))
	byID := make(map[string]*scheduledAuth, len(v.flat))
	for _, entry := range v.flat {
		if entry == nil || entry.auth == nil {
			continue
		}
		if predicate != nil && !predicate(entry) {
			continue
		}
		auths = append(auths, entry.auth)
		byID[entry.auth.ID] = entry
	}
	picked := pickSoonestResetAuth(auths, model, now)
	if picked == nil {
		return nil
	}
	return byID[picked.ID]
}
