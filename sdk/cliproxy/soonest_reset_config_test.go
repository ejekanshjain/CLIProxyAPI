package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset"},
	})
	if state.strategy != "soonest-reset" {
		t.Fatalf("strategy = %q, want soonest-reset", state.strategy)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", newRoutingSelector(state))
	}

	affinity := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:        "soonest-reset",
			SessionAffinity: true,
		},
	})
	if _, ok := newRoutingSelector(affinity).(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("affinity selector type = %T, want *auth.SessionAffinitySelector", newRoutingSelector(affinity))
	}

	defaultState := normalizedRoutingRuntimeState(&internalconfig.Config{})
	if defaultState.strategy != "round-robin" {
		t.Fatalf("default strategy = %q, want round-robin", defaultState.strategy)
	}
	if _, ok := newRoutingSelector(defaultState).(*coreauth.RoundRobinSelector); !ok {
		t.Fatalf("default selector type = %T, want *auth.RoundRobinSelector", newRoutingSelector(defaultState))
	}
}
