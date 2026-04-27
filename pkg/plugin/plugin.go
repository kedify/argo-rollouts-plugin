package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	plugintypes "github.com/argoproj/argo-rollouts/utils/plugin/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

const (
	// pluginName is the key under which Argo Rollouts looks up this plugin's
	// config in `spec.strategy.canary.trafficRouting.plugins`. Argo Rollouts
	// requires plugin names to be in `<namespace>/<name>` format.
	pluginName = "kedify/http"

	weightedBackendsAnnotation = "http.kedify.io/weighted-backends"

	// k8sCallTimeout bounds individual apiserver calls so a stalled connection
	// can't hang a rollout step indefinitely. Rollouts retries on RpcError, so
	// timing out here lets the controller try again on the next reconcile.
	k8sCallTimeout = 10 * time.Second
)

// notInitializedErr is returned when a plugin method is invoked before
// InitPlugin has set up the dynamic client (or after it failed silently).
func notInitializedErr() plugintypes.RpcError {
	return plugintypes.RpcError{ErrorString: fmt.Sprintf("plugin %q not initialized: InitPlugin was not called or failed", pluginName)}
}

// additionalDestinationsErr is returned by SetWeight/VerifyWeight/UpdateHash
// when Argo Rollouts passes experiment/analysis destinations the kedify/http
// plugin doesn't implement. Failing loudly here is preferred over silently
// producing a routing config that doesn't reflect what the controller asked
// for.
func additionalDestinationsErr(n int) plugintypes.RpcError {
	return plugintypes.RpcError{ErrorString: fmt.Sprintf(
		"plugin %q does not support additionalDestinations (experiment/analysis traffic); got %d destinations",
		pluginName, n)}
}

var httpsoGVR = schema.GroupVersionResource{
	Group:    "http.keda.sh",
	Version:  "v1alpha1",
	Resource: "httpscaledobjects",
}

// WeightedBackend represents a single backend in a weighted traffic split
type WeightedBackend struct {
	Service string `json:"service" yaml:"service"`
	Weight  uint32 `json:"weight" yaml:"weight"`
}

// KedifyPluginConfig is the plugin configuration from the Rollout CR
type KedifyPluginConfig struct {
	HTTPScaledObjectName string `json:"httpScaledObjectName"`
}

// KedifyPlugin implements the Argo Rollouts TrafficRouterPlugin interface
type KedifyPlugin struct {
	client dynamic.Interface
}

func (p *KedifyPlugin) InitPlugin() plugintypes.RpcError {
	config, err := rest.InClusterConfig()
	if err != nil {
		return plugintypes.RpcError{ErrorString: fmt.Sprintf("failed to get in-cluster config: %v", err)}
	}
	p.client, err = dynamic.NewForConfig(config)
	if err != nil {
		return plugintypes.RpcError{ErrorString: fmt.Sprintf("failed to create dynamic client: %v", err)}
	}
	slog.Info("KedifyPlugin initialized")
	return plugintypes.RpcError{}
}

func (p *KedifyPlugin) Type() string {
	return "Kedify"
}

// SetWeight matches the Argo Rollouts TrafficRouterPlugin interface signature; line length is dictated by the upstream API.
//
//nolint:lll
func (p *KedifyPlugin) SetWeight(rollout *v1alpha1.Rollout, desiredWeight int32, additionalDestinations []v1alpha1.WeightDestination) plugintypes.RpcError {
	if p.client == nil {
		return notInitializedErr()
	}
	if desiredWeight < 0 || desiredWeight > 100 {
		return plugintypes.RpcError{ErrorString: fmt.Sprintf("desiredWeight must be in [0,100], got %d", desiredWeight)}
	}
	if len(additionalDestinations) > 0 {
		return additionalDestinationsErr(len(additionalDestinations))
	}
	cfg, err := parsePluginConfig(rollout)
	if err != nil {
		return plugintypes.RpcError{ErrorString: err.Error()}
	}

	stableService := rollout.Spec.Strategy.Canary.StableService
	canaryService := rollout.Spec.Strategy.Canary.CanaryService
	if stableService == "" || canaryService == "" {
		return plugintypes.RpcError{ErrorString: "stableService and canaryService must be set in rollout spec"}
	}

	namespace := rollout.Namespace
	slog.Info("SetWeight", "httpso", cfg.HTTPScaledObjectName, "namespace", namespace,
		"desiredWeight", desiredWeight, "stable", stableService, "canary", canaryService)

	var patch map[string]interface{}
	if desiredWeight == 0 {
		// remove annotation when weight is 0 (promotion complete)
		patch = map[string]interface{}{
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{
					weightedBackendsAnnotation: nil,
				},
			},
		}
	} else {
		backends := []WeightedBackend{
			{Service: stableService, Weight: uint32(100 - desiredWeight)},
			{Service: canaryService, Weight: uint32(desiredWeight)},
		}
		backendsYAML, err := yaml.Marshal(backends)
		if err != nil {
			return plugintypes.RpcError{ErrorString: fmt.Sprintf("failed to marshal weighted backends: %v", err)}
		}
		patch = map[string]interface{}{
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{
					weightedBackendsAnnotation: string(backendsYAML),
				},
			},
		}
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return plugintypes.RpcError{ErrorString: fmt.Sprintf("failed to marshal patch: %v", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), k8sCallTimeout)
	defer cancel()
	_, err = p.client.Resource(httpsoGVR).Namespace(namespace).Patch(
		ctx,
		cfg.HTTPScaledObjectName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return plugintypes.RpcError{ErrorString: fmt.Sprintf("failed to patch HTTPSO %s/%s: %v", namespace, cfg.HTTPScaledObjectName, err)}
	}

	slog.Info("SetWeight completed", "httpso", cfg.HTTPScaledObjectName, "desiredWeight", desiredWeight)
	return plugintypes.RpcError{}
}

// VerifyWeight matches the Argo Rollouts TrafficRouterPlugin interface signature; line length is dictated by the upstream API.
//
//nolint:lll
func (p *KedifyPlugin) VerifyWeight(rollout *v1alpha1.Rollout, desiredWeight int32, additionalDestinations []v1alpha1.WeightDestination) (plugintypes.RpcVerified, plugintypes.RpcError) {
	if p.client == nil {
		return plugintypes.NotVerified, notInitializedErr()
	}
	if desiredWeight < 0 || desiredWeight > 100 {
		return plugintypes.NotVerified, plugintypes.RpcError{ErrorString: fmt.Sprintf("desiredWeight must be in [0,100], got %d", desiredWeight)}
	}
	if len(additionalDestinations) > 0 {
		return plugintypes.NotVerified, additionalDestinationsErr(len(additionalDestinations))
	}
	cfg, err := parsePluginConfig(rollout)
	if err != nil {
		return plugintypes.NotVerified, plugintypes.RpcError{ErrorString: err.Error()}
	}

	canaryService := rollout.Spec.Strategy.Canary.CanaryService
	if canaryService == "" {
		return plugintypes.NotVerified, plugintypes.RpcError{ErrorString: "canaryService must be set in rollout spec"}
	}

	namespace := rollout.Namespace
	ctx, cancel := context.WithTimeout(context.Background(), k8sCallTimeout)
	defer cancel()
	httpso, err := p.client.Resource(httpsoGVR).Namespace(namespace).Get(
		ctx,
		cfg.HTTPScaledObjectName,
		metav1.GetOptions{},
	)
	if err != nil {
		return plugintypes.NotVerified, plugintypes.RpcError{ErrorString: fmt.Sprintf("failed to get HTTPSO %s/%s: %v", namespace, cfg.HTTPScaledObjectName, err)}
	}

	annotations := httpso.GetAnnotations()
	if desiredWeight == 0 {
		// weight 0 means promotion is complete, annotation should be removed
		if _, exists := annotations[weightedBackendsAnnotation]; !exists {
			return plugintypes.Verified, plugintypes.RpcError{}
		}
		return plugintypes.NotVerified, plugintypes.RpcError{}
	}

	annotationValue, exists := annotations[weightedBackendsAnnotation]
	if !exists {
		return plugintypes.NotVerified, plugintypes.RpcError{}
	}

	var backends []WeightedBackend
	if err := yaml.Unmarshal([]byte(annotationValue), &backends); err != nil {
		return plugintypes.NotVerified, plugintypes.RpcError{ErrorString: fmt.Sprintf("failed to parse annotation: %v", err)}
	}

	for _, b := range backends {
		if b.Service == canaryService && int32(b.Weight) == desiredWeight {
			return plugintypes.Verified, plugintypes.RpcError{}
		}
	}

	return plugintypes.NotVerified, plugintypes.RpcError{}
}

func (p *KedifyPlugin) RemoveManagedRoutes(rollout *v1alpha1.Rollout) plugintypes.RpcError {
	if p.client == nil {
		return notInitializedErr()
	}
	cfg, err := parsePluginConfig(rollout)
	if err != nil {
		return plugintypes.RpcError{ErrorString: err.Error()}
	}

	namespace := rollout.Namespace
	slog.Info("RemoveManagedRoutes", "httpso", cfg.HTTPScaledObjectName, "namespace", namespace)

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				weightedBackendsAnnotation: nil,
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return plugintypes.RpcError{ErrorString: fmt.Sprintf("failed to marshal patch: %v", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), k8sCallTimeout)
	defer cancel()
	_, err = p.client.Resource(httpsoGVR).Namespace(namespace).Patch(
		ctx,
		cfg.HTTPScaledObjectName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return plugintypes.RpcError{ErrorString: fmt.Sprintf("failed to patch HTTPSO %s/%s: %v", namespace, cfg.HTTPScaledObjectName, err)}
	}

	return plugintypes.RpcError{}
}

// UpdateHash matches the Argo Rollouts TrafficRouterPlugin interface signature; line length is dictated by the upstream API.
//
//nolint:lll
func (p *KedifyPlugin) UpdateHash(rollout *v1alpha1.Rollout, canaryHash string, stableHash string, additionalDestinations []v1alpha1.WeightDestination) plugintypes.RpcError {
	if len(additionalDestinations) > 0 {
		return additionalDestinationsErr(len(additionalDestinations))
	}
	return plugintypes.RpcError{}
}

// SetHeaderRoute fails when invoked with a non-nil route — the kedify/http
// plugin does not implement header-based routing and returning success would
// cause the controller to believe a route was configured when it wasn't.
func (p *KedifyPlugin) SetHeaderRoute(rollout *v1alpha1.Rollout, setHeaderRoute *v1alpha1.SetHeaderRoute) plugintypes.RpcError {
	if setHeaderRoute == nil {
		return plugintypes.RpcError{}
	}
	return plugintypes.RpcError{ErrorString: fmt.Sprintf(
		"plugin %q does not support header-based routing (rollout %s/%s)",
		pluginName, rollout.Namespace, rollout.Name)}
}

// SetMirrorRoute fails when invoked with a non-nil route — the kedify/http
// plugin does not implement traffic mirroring.
func (p *KedifyPlugin) SetMirrorRoute(rollout *v1alpha1.Rollout, setMirrorRoute *v1alpha1.SetMirrorRoute) plugintypes.RpcError {
	if setMirrorRoute == nil {
		return plugintypes.RpcError{}
	}
	return plugintypes.RpcError{ErrorString: fmt.Sprintf(
		"plugin %q does not support traffic mirroring (rollout %s/%s)",
		pluginName, rollout.Namespace, rollout.Name)}
}

// parsePluginConfig extracts the Kedify plugin config from the Rollout spec
func parsePluginConfig(rollout *v1alpha1.Rollout) (*KedifyPluginConfig, error) {
	if rollout.Spec.Strategy.Canary == nil ||
		rollout.Spec.Strategy.Canary.TrafficRouting == nil ||
		rollout.Spec.Strategy.Canary.TrafficRouting.Plugins == nil {
		return nil, fmt.Errorf("rollout %s/%s has no traffic routing plugins configured", rollout.Namespace, rollout.Name)
	}

	pluginConfig, ok := rollout.Spec.Strategy.Canary.TrafficRouting.Plugins[pluginName]
	if !ok {
		return nil, fmt.Errorf("rollout %s/%s has no %q plugin config", rollout.Namespace, rollout.Name, pluginName)
	}

	cfg := &KedifyPluginConfig{}
	// Plugin config comes as json.RawMessage
	if err := json.Unmarshal(pluginConfig, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %q plugin config: %w", pluginName, err)
	}
	if cfg.HTTPScaledObjectName == "" {
		return nil, fmt.Errorf("%q plugin config missing httpScaledObjectName", pluginName)
	}

	return cfg, nil
}

// SetClient allows injecting a client for testing
func (p *KedifyPlugin) SetClient(client dynamic.Interface) {
	p.client = client
}

// GetHTTPSO retrieves the HTTPSO for testing/debugging
func (p *KedifyPlugin) GetHTTPSO(namespace, name string) (*unstructured.Unstructured, error) {
	if p.client == nil {
		return nil, fmt.Errorf("plugin %q not initialized: InitPlugin was not called or failed", pluginName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), k8sCallTimeout)
	defer cancel()
	return p.client.Resource(httpsoGVR).Namespace(namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
}
