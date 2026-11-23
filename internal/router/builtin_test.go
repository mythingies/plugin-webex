package router

import (
	"io/fs"
	"testing"

	pluginwebex "github.com/mythingies/plugin-webex"
)

func TestBuiltinConfig(t *testing.T) {
	// Parse directly so a broken embedded file fails here instead of
	// silently degrading to DefaultConfig.
	cfg, err := parseConfig(pluginwebex.DefaultRoutes)
	if err != nil {
		t.Fatalf("embedded .webex-agents.yml is invalid: %v", err)
	}
	if len(cfg.Routes) == 0 {
		t.Fatal("embedded config has no routes")
	}
	if got := BuiltinConfig(); len(got.Routes) != len(cfg.Routes) {
		t.Errorf("BuiltinConfig returned %d routes, want %d", len(got.Routes), len(cfg.Routes))
	}

	// Every routed agent should have a shipped playbook, or a message routed
	// to it reaches Claude with no instructions.
	for _, r := range cfg.Routes {
		if _, err := fs.Stat(pluginwebex.Playbooks, "agents/"+r.Agent+".md"); err != nil {
			t.Errorf("route agent %q has no embedded playbook: %v", r.Agent, err)
		}
	}
}

func TestBuiltinConfig_RoutesDM(t *testing.T) {
	r := NewRouter(BuiltinConfig(), "")
	res := r.Route(InboundMessage{RoomType: "direct", Text: "hi"})
	if res == nil || res.Agent != "dm-responder" {
		t.Fatalf("expected a DM to route to an agent with built-in config, got %+v", res)
	}
}
