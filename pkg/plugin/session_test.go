package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	plugintypes "github.com/argoproj/argo-rollouts/utils/plugin/types"
)

func stickyRolloutConfig(t *testing.T) (*KedifyPluginConfig, *SessionPersistence) {
	t.Helper()
	session := &SessionPersistence{Type: "Cookie", AbsoluteTimeout: "15m", Cookie: SessionCookie{Name: "app-session"}}
	return &KedifyPluginConfig{HTTPScaledObjectName: "myapp", SessionPersistence: session}, session
}

func TestSessionPersistenceLifecycle(t *testing.T) {
	rollout := newTestRollout("myapp-stable", "myapp-canary")
	rollout.Annotations = map[string]string{"rollout.argoproj.io/revision": "2"}
	cfg, _ := stickyRolloutConfig(t)
	rollout.Spec.Strategy.Canary.TrafficRouting.Plugins[pluginName] = mustMarshalPluginCfg(*cfg)
	p := newFakePlugin(newFakeHTTPSO())
	if err := p.SetWeight(rollout, 20, nil); err.HasError() {
		t.Fatal(err.ErrorString)
	}
	obj, err := p.GetHTTPSO("default", "myapp")
	if err != nil {
		t.Fatal(err)
	}
	first := obj.GetAnnotations()[sessionPersistenceAnnotation]
	var session SessionPersistence
	if err := json.Unmarshal([]byte(first), &session); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(session.Cookie.Name, "app-session-") || session.AbsoluteTimeout != "15m" {
		t.Fatalf("unexpected session config: %+v", session)
	}
	if verified, err := p.VerifyWeight(rollout, 20, nil); err.HasError() || verified != plugintypes.Verified {
		t.Fatalf("VerifyWeight: %v %v", verified, err)
	}
	if err := p.SetWeight(rollout, 80, nil); err.HasError() {
		t.Fatal(err.ErrorString)
	}
	obj, _ = p.GetHTTPSO("default", "myapp")
	if obj.GetAnnotations()[sessionPersistenceAnnotation] != first {
		t.Fatal("weight progression changed cookie identity")
	}
	rollout.Status.CurrentPodHash = "status-caught-up"
	if verified, _ := p.VerifyWeight(rollout, 80, nil); verified != plugintypes.Verified {
		t.Fatal("status synchronization changed cookie identity")
	}
	rollout.Annotations["rollout.argoproj.io/revision"] = "3"
	if verified, _ := p.VerifyWeight(rollout, 80, nil); verified != plugintypes.NotVerified {
		t.Fatal("stale revision cookie was accepted")
	}
	if err := p.SetWeight(rollout, 80, nil); err.HasError() {
		t.Fatal(err.ErrorString)
	}
	obj, _ = p.GetHTTPSO("default", "myapp")
	if obj.GetAnnotations()[sessionPersistenceAnnotation] == first {
		t.Fatal("new revision reused cookie identity")
	}
	if err := p.SetWeight(rollout, 0, nil); err.HasError() {
		t.Fatal(err.ErrorString)
	}
	obj, _ = p.GetHTTPSO("default", "myapp")
	if obj.GetAnnotations()[sessionPersistenceAnnotation] != "" || obj.GetAnnotations()[weightedBackendsAnnotation] != "" {
		t.Fatal("weight zero did not remove routing and persistence")
	}
	if err := p.SetWeight(rollout, 50, nil); err.HasError() {
		t.Fatal(err.ErrorString)
	}
	if err := p.RemoveManagedRoutes(rollout); err.HasError() {
		t.Fatal(err.ErrorString)
	}
	obj, _ = p.GetHTTPSO("default", "myapp")
	if obj.GetAnnotations()[sessionPersistenceAnnotation] != "" {
		t.Fatal("cleanup left session persistence behind")
	}
}

func TestSessionPersistenceValidation(t *testing.T) {
	for name, change := range map[string]func(*SessionPersistence){
		"type":       func(s *SessionPersistence) { s.Type = "Header" },
		"name":       func(s *SessionPersistence) { s.Cookie.Name = "bad name" },
		"timeout":    func(s *SessionPersistence) { s.AbsoluteTimeout = "invalid" },
		"subsecond":  func(s *SessionPersistence) { s.AbsoluteTimeout = "500ms" },
		"fractional": func(s *SessionPersistence) { s.AbsoluteTimeout = "1.5s" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, session := stickyRolloutConfig(t)
			change(session)
			rollout := newTestRollout("stable", "canary")
			rollout.Annotations = map[string]string{"rollout.argoproj.io/revision": "2"}
			if _, err := sessionAnnotation(rollout, cfg); err == nil {
				t.Fatal("invalid session persistence was accepted")
			}
		})
	}
	cfg, _ := stickyRolloutConfig(t)
	if _, err := sessionAnnotation(newTestRollout("stable", "canary"), cfg); err == nil {
		t.Fatal("missing revision was accepted")
	}
}

func TestDisablingSessionPersistenceClearsAnnotation(t *testing.T) {
	obj := newFakeHTTPSO()
	obj.SetAnnotations(map[string]string{sessionPersistenceAnnotation: "stale"})
	p := newFakePlugin(obj)
	if err := p.SetWeight(newTestRollout("stable", "canary"), 50, nil); err.HasError() {
		t.Fatal(err.ErrorString)
	}
	obj, _ = p.GetHTTPSO("default", "myapp")
	if _, exists := obj.GetAnnotations()[sessionPersistenceAnnotation]; exists {
		t.Fatal("disabled persistence annotation remains")
	}
}
