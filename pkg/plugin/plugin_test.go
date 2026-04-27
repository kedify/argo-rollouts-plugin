package plugin

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"
)

// newFakeHTTPSO returns an unstructured HTTPScaledObject in default/myapp for use in tests.
func newFakeHTTPSO() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "http.keda.sh/v1alpha1",
			"kind":       "HTTPScaledObject",
			"metadata": map[string]interface{}{
				"name":      "myapp",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"scaleTargetRef": map[string]interface{}{
					"service": "myapp-stable",
				},
			},
		},
	}
}

// mustMarshalPluginCfg marshals a KedifyPluginConfig in test setup. KedifyPluginConfig
// has only string fields so json.Marshal cannot fail; if it ever does, the test crashes
// loudly rather than silently producing an empty payload.
func mustMarshalPluginCfg(cfg KedifyPluginConfig) json.RawMessage {
	out, err := json.Marshal(cfg)
	if err != nil {
		panic(fmt.Sprintf("marshal KedifyPluginConfig: %v", err))
	}
	return out
}

// newTestRollout returns a Rollout that targets HTTPSO default/myapp via the kedify/http plugin.
func newTestRollout(stableService, canaryService string) *v1alpha1.Rollout {
	pluginCfg := mustMarshalPluginCfg(KedifyPluginConfig{HTTPScaledObjectName: "myapp"})
	return &v1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rollout",
			Namespace: "default",
		},
		Spec: v1alpha1.RolloutSpec{
			Strategy: v1alpha1.RolloutStrategy{
				Canary: &v1alpha1.CanaryStrategy{
					StableService: stableService,
					CanaryService: canaryService,
					TrafficRouting: &v1alpha1.RolloutTrafficRouting{
						Plugins: map[string]json.RawMessage{
							pluginName: pluginCfg,
						},
					},
				},
			},
		},
	}
}

func newFakePlugin(httpso *unstructured.Unstructured) *KedifyPlugin {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			httpsoGVR: "HTTPScaledObjectList",
		},
		httpso,
	)
	p := &KedifyPlugin{}
	p.SetClient(client)
	return p
}

func TestSetWeight(t *testing.T) {
	httpso := newFakeHTTPSO()
	p := newFakePlugin(httpso)
	rollout := newTestRollout("myapp-stable", "myapp-canary")

	// Set weight to 20 (canary gets 20%, stable gets 80%)
	rpcErr := p.SetWeight(rollout, 20, nil)
	if rpcErr.HasError() {
		t.Fatalf("SetWeight failed: %s", rpcErr.ErrorString)
	}

	// Verify the annotation was set
	obj, err := p.GetHTTPSO("default", "myapp")
	if err != nil {
		t.Fatalf("failed to get HTTPSO: %v", err)
	}

	annotations := obj.GetAnnotations()
	annotationValue, exists := annotations[weightedBackendsAnnotation]
	if !exists {
		t.Fatal("weighted backends annotation not found")
	}

	var backends []WeightedBackend
	if err := yaml.Unmarshal([]byte(annotationValue), &backends); err != nil {
		t.Fatalf("failed to parse annotation: %v", err)
	}

	if len(backends) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(backends))
	}
	if backends[0].Service != "myapp-stable" || backends[0].Weight != 80 {
		t.Errorf("expected stable backend {myapp-stable, 80}, got {%s, %d}", backends[0].Service, backends[0].Weight)
	}
	if backends[1].Service != "myapp-canary" || backends[1].Weight != 20 {
		t.Errorf("expected canary backend {myapp-canary, 20}, got {%s, %d}", backends[1].Service, backends[1].Weight)
	}
}

func TestSetWeightZeroRemovesAnnotation(t *testing.T) {
	httpso := newFakeHTTPSO()
	p := newFakePlugin(httpso)
	rollout := newTestRollout("myapp-stable", "myapp-canary")

	// First set a weight
	rpcErr := p.SetWeight(rollout, 50, nil)
	if rpcErr.HasError() {
		t.Fatalf("SetWeight(50) failed: %s", rpcErr.ErrorString)
	}

	// Then set weight to 0 (promotion complete)
	rpcErr = p.SetWeight(rollout, 0, nil)
	if rpcErr.HasError() {
		t.Fatalf("SetWeight(0) failed: %s", rpcErr.ErrorString)
	}

	obj, err := p.GetHTTPSO("default", "myapp")
	if err != nil {
		t.Fatalf("failed to get HTTPSO: %v", err)
	}

	annotations := obj.GetAnnotations()
	if _, exists := annotations[weightedBackendsAnnotation]; exists {
		t.Error("expected annotation to be removed after SetWeight(0)")
	}
}

func TestVerifyWeight(t *testing.T) {
	httpso := newFakeHTTPSO()
	p := newFakePlugin(httpso)
	rollout := newTestRollout("myapp-stable", "myapp-canary")

	// Set weight
	rpcErr := p.SetWeight(rollout, 30, nil)
	if rpcErr.HasError() {
		t.Fatalf("SetWeight failed: %s", rpcErr.ErrorString)
	}

	// Verify the correct weight
	verified, rpcErr := p.VerifyWeight(rollout, 30, nil)
	if rpcErr.HasError() {
		t.Fatalf("VerifyWeight failed: %s", rpcErr.ErrorString)
	}
	if v := verified.IsVerified(); v == nil || !*v {
		t.Error("expected weight to be verified")
	}

	// Verify incorrect weight
	verified, rpcErr = p.VerifyWeight(rollout, 50, nil)
	if rpcErr.HasError() {
		t.Fatalf("VerifyWeight failed: %s", rpcErr.ErrorString)
	}
	if v := verified.IsVerified(); v != nil && *v {
		t.Error("expected weight to NOT be verified for wrong value")
	}
}

func TestRemoveManagedRoutes(t *testing.T) {
	httpso := newFakeHTTPSO()
	// Pre-set the annotation
	httpso.SetAnnotations(map[string]string{
		weightedBackendsAnnotation: "- service: myapp-stable\n  weight: 80\n- service: myapp-canary\n  weight: 20\n",
	})
	p := newFakePlugin(httpso)
	rollout := newTestRollout("myapp-stable", "myapp-canary")

	rpcErr := p.RemoveManagedRoutes(rollout)
	if rpcErr.HasError() {
		t.Fatalf("RemoveManagedRoutes failed: %s", rpcErr.ErrorString)
	}

	obj, err := p.GetHTTPSO("default", "myapp")
	if err != nil {
		t.Fatalf("failed to get HTTPSO: %v", err)
	}

	annotations := obj.GetAnnotations()
	if _, exists := annotations[weightedBackendsAnnotation]; exists {
		t.Error("expected weighted backends annotation to be removed")
	}
}

func TestParsePluginConfig(t *testing.T) {
	tests := []struct {
		name    string
		rollout *v1alpha1.Rollout
		wantErr bool
	}{
		{
			name:    "valid config",
			rollout: newTestRollout("stable", "canary"),
			wantErr: false,
		},
		{
			name: "missing plugin config",
			rollout: &v1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: v1alpha1.RolloutSpec{
					Strategy: v1alpha1.RolloutStrategy{
						Canary: &v1alpha1.CanaryStrategy{},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "empty httpScaledObjectName",
			rollout: &v1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: v1alpha1.RolloutSpec{
					Strategy: v1alpha1.RolloutStrategy{
						Canary: &v1alpha1.CanaryStrategy{
							TrafficRouting: &v1alpha1.RolloutTrafficRouting{
								Plugins: map[string]json.RawMessage{pluginName: mustMarshalPluginCfg(KedifyPluginConfig{})},
							},
						},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePluginConfig(tt.rollout)
			if (err != nil) != tt.wantErr {
				t.Errorf("parsePluginConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestSetWeightMissingServices verifies error when stable/canary services not set
func TestSetWeightMissingServices(t *testing.T) {
	httpso := newFakeHTTPSO()
	p := newFakePlugin(httpso)

	cfg := mustMarshalPluginCfg(KedifyPluginConfig{HTTPScaledObjectName: "myapp"})
	rollout := &v1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.RolloutSpec{
			Strategy: v1alpha1.RolloutStrategy{
				Canary: &v1alpha1.CanaryStrategy{
					TrafficRouting: &v1alpha1.RolloutTrafficRouting{
						Plugins: map[string]json.RawMessage{pluginName: cfg},
					},
				},
			},
		},
	}

	rpcErr := p.SetWeight(rollout, 20, nil)
	if !rpcErr.HasError() {
		t.Error("expected error when stableService/canaryService not set")
	}
}

// TestVerifyWeightZero verifies that weight 0 checks annotation is removed
func TestVerifyWeightZero(t *testing.T) {
	httpso := newFakeHTTPSO()
	p := newFakePlugin(httpso)
	rollout := newTestRollout("myapp-stable", "myapp-canary")

	// No annotation set, verify weight 0 should pass
	verified, rpcErr := p.VerifyWeight(rollout, 0, nil)
	if rpcErr.HasError() {
		t.Fatalf("VerifyWeight failed: %s", rpcErr.ErrorString)
	}
	if v := verified.IsVerified(); v == nil || !*v {
		t.Error("expected weight 0 to be verified when annotation is absent")
	}
}

// TestNoopMethods ensures no-op methods don't error
func TestNoopMethods(t *testing.T) {
	p := &KedifyPlugin{}
	rollout := newTestRollout("stable", "canary")

	if err := p.UpdateHash(rollout, "abc", "def", nil); err.HasError() {
		t.Errorf("UpdateHash returned error: %s", err.ErrorString)
	}
	if err := p.SetHeaderRoute(rollout, nil); err.HasError() {
		t.Errorf("SetHeaderRoute returned error: %s", err.ErrorString)
	}
	if err := p.SetMirrorRoute(rollout, nil); err.HasError() {
		t.Errorf("SetMirrorRoute returned error: %s", err.ErrorString)
	}
}

// TestSetWeightInvalidWeight covers the [0,100] range guard.
func TestSetWeightInvalidWeight(t *testing.T) {
	p := newFakePlugin(newFakeHTTPSO())
	rollout := newTestRollout("myapp-stable", "myapp-canary")
	for _, w := range []int32{-1, 101, 200} {
		if err := p.SetWeight(rollout, w, nil); !err.HasError() {
			t.Errorf("SetWeight(%d) expected error, got success", w)
		}
	}
}

// TestSetWeightAdditionalDestinationsRejected ensures unsupported features
// fail loudly rather than silently dropping them.
func TestSetWeightAdditionalDestinationsRejected(t *testing.T) {
	p := newFakePlugin(newFakeHTTPSO())
	rollout := newTestRollout("myapp-stable", "myapp-canary")
	dests := []v1alpha1.WeightDestination{{ServiceName: "extra", Weight: 5}}
	if err := p.SetWeight(rollout, 20, dests); !err.HasError() {
		t.Error("SetWeight with additionalDestinations expected error, got success")
	}
}

// TestVerifyWeightAdditionalDestinationsRejected mirrors SetWeight's check.
func TestVerifyWeightAdditionalDestinationsRejected(t *testing.T) {
	p := newFakePlugin(newFakeHTTPSO())
	rollout := newTestRollout("myapp-stable", "myapp-canary")
	dests := []v1alpha1.WeightDestination{{ServiceName: "extra", Weight: 5}}
	if _, err := p.VerifyWeight(rollout, 20, dests); !err.HasError() {
		t.Error("VerifyWeight with additionalDestinations expected error, got success")
	}
}

// TestVerifyWeightInvalidWeight covers the [0,100] range guard.
func TestVerifyWeightInvalidWeight(t *testing.T) {
	p := newFakePlugin(newFakeHTTPSO())
	rollout := newTestRollout("myapp-stable", "myapp-canary")
	for _, w := range []int32{-1, 101} {
		if _, err := p.VerifyWeight(rollout, w, nil); !err.HasError() {
			t.Errorf("VerifyWeight(%d) expected error, got success", w)
		}
	}
}

// TestVerifyWeightMissingCanaryService ensures rollouts without canaryService surface a clear error.
func TestVerifyWeightMissingCanaryService(t *testing.T) {
	p := newFakePlugin(newFakeHTTPSO())
	rollout := newTestRollout("myapp-stable", "")
	if _, err := p.VerifyWeight(rollout, 20, nil); !err.HasError() {
		t.Error("VerifyWeight with empty canaryService expected error, got success")
	}
}

// TestNotInitialized ensures methods don't panic when InitPlugin hasn't run.
func TestNotInitialized(t *testing.T) {
	p := &KedifyPlugin{} // no client
	rollout := newTestRollout("myapp-stable", "myapp-canary")
	if err := p.SetWeight(rollout, 20, nil); !err.HasError() {
		t.Error("SetWeight on uninitialized plugin expected error")
	}
	if _, err := p.VerifyWeight(rollout, 20, nil); !err.HasError() {
		t.Error("VerifyWeight on uninitialized plugin expected error")
	}
	if err := p.RemoveManagedRoutes(rollout); !err.HasError() {
		t.Error("RemoveManagedRoutes on uninitialized plugin expected error")
	}
	if _, err := p.GetHTTPSO("default", "myapp"); err == nil {
		t.Error("GetHTTPSO on uninitialized plugin expected error")
	}
}

// TestSetHeaderRouteRejectsNonNil ensures the unsupported feature fails loudly.
func TestSetHeaderRouteRejectsNonNil(t *testing.T) {
	p := newFakePlugin(newFakeHTTPSO())
	rollout := newTestRollout("myapp-stable", "myapp-canary")
	if err := p.SetHeaderRoute(rollout, &v1alpha1.SetHeaderRoute{Name: "x"}); !err.HasError() {
		t.Error("SetHeaderRoute with non-nil arg expected error, got success")
	}
}

// TestSetMirrorRouteRejectsNonNil ensures the unsupported feature fails loudly.
func TestSetMirrorRouteRejectsNonNil(t *testing.T) {
	p := newFakePlugin(newFakeHTTPSO())
	rollout := newTestRollout("myapp-stable", "myapp-canary")
	if err := p.SetMirrorRoute(rollout, &v1alpha1.SetMirrorRoute{Name: "x"}); !err.HasError() {
		t.Error("SetMirrorRoute with non-nil arg expected error, got success")
	}
}

// TestUpdateHashAdditionalDestinationsRejected mirrors the SetWeight/VerifyWeight guard
// so any experiment/analysis destination passed to UpdateHash also fails loudly.
func TestUpdateHashAdditionalDestinationsRejected(t *testing.T) {
	p := newFakePlugin(newFakeHTTPSO())
	rollout := newTestRollout("myapp-stable", "myapp-canary")
	dests := []v1alpha1.WeightDestination{{ServiceName: "extra", Weight: 5}}
	if err := p.UpdateHash(rollout, "abc", "def", dests); !err.HasError() {
		t.Error("UpdateHash with additionalDestinations expected error, got success")
	}
	// And confirm the empty case is still a no-op success.
	if err := p.UpdateHash(rollout, "abc", "def", nil); err.HasError() {
		t.Errorf("UpdateHash without additionalDestinations should succeed, got %s", err.ErrorString)
	}
}

// compile-time check: KedifyPlugin implements the expected patch interface
var _ interface {
	SetClient(dynamic.Interface)
} = &KedifyPlugin{}
