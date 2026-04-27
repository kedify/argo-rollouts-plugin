# Kedify Argo Rollouts Plugin

A [traffic-router plugin](https://argo-rollouts.readthedocs.io/en/stable/features/traffic-management/plugins/)
for [Argo Rollouts](https://argoproj.github.io/argo-rollouts/) that drives
canary traffic splitting through Kedify's HTTP autoscaling stack.

Argo Rollouts owns the canary steps (image promotion, weight progression,
pause conditions). This plugin translates each `setWeight` step into a YAML
annotation on the matching `HTTPScaledObject`, which the Kedify interceptor
picks up and turns into Envoy `weighted_clusters`. Ingress autowire keeps the
upstream Ingress / ALB pointed at `kedify-proxy`, so traffic splitting happens
inside the interceptor rather than the load balancer.

## How it fits together

```
Argo Rollouts → SetWeight(N)
   ↓ patches
HTTPScaledObject annotation: http.kedify.io/weighted-backends
   - service: stable
     weight: 100-N
   - service: canary
     weight: N
   ↓ observed by
Kedify interceptor → Envoy WeightedClusters
   ↓
kedify-proxy splits traffic between stable / canary services
```

When `setWeight` reaches `0` the plugin removes the annotation and the
interceptor reverts to a single cluster.

## Install

The plugin is registered with Argo Rollouts under the name **`kedify/http`**.

Argo Rollouts downloads plugin binaries on controller startup from the URL you
configure in `argo-rollouts-config`. Pre-built binaries for `linux/amd64`,
`linux/arm64`, `darwin/amd64` and `darwin/arm64` are published as
[GitHub release assets](https://github.com/kedify/argo-rollouts-plugin/releases),
together with a `checksums.txt` file you can use to pin the binary by SHA-256.

### 1. Configure the plugin in Argo Rollouts

Patch the `argo-rollouts-config` ConfigMap in the `argo-rollouts` namespace:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argo-rollouts-config
  namespace: argo-rollouts
data:
  trafficRouterPlugins: |
    - name: "kedify/http"
      location: "https://github.com/kedify/argo-rollouts-plugin/releases/download/v0.1.0/rollouts-plugin-kedify-linux-amd64"
      sha256: "<paste from checksums.txt>"
```

Pick the asset that matches the OS / arch of the node running the
`argo-rollouts` controller. For multi-arch clusters, point to the matching
release asset for each platform you support — Argo Rollouts caches the
binary on the controller pod's local disk between restarts.

### 2. Restart the argo-rollouts controller

```sh
kubectl -n argo-rollouts rollout restart deploy/argo-rollouts
```

The controller downloads the binary, verifies the SHA-256, and serves it from
`$HOME/plugin-bin/kedify/http`. No init container, no custom image, no
deployment patching needed — the standard `argo-rollouts` Deployment already
mounts the `plugin-bin` `emptyDir` the plugin runtime expects.

### 3. Grant RBAC

The plugin patches `HTTPScaledObject` resources from the `argo-rollouts`
ServiceAccount, so it needs explicit permission:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: argo-rollouts-kedify-http
rules:
  - apiGroups: ["http.keda.sh"]
    resources: ["httpscaledobjects"]
    verbs: ["get", "list", "watch", "patch", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: argo-rollouts-kedify-http
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: argo-rollouts-kedify-http
subjects:
  - kind: ServiceAccount
    name: argo-rollouts
    namespace: argo-rollouts
```

## Use the plugin in a Rollout

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: myapp
spec:
  strategy:
    canary:
      stableService: myapp-stable
      canaryService: myapp-canary
      trafficRouting:
        plugins:
          kedify/http:
            httpScaledObjectName: myapp   # HTTPSO to patch
      steps:
        - setWeight: 20
        - pause: {}
        - setWeight: 50
        - pause: {duration: 30s}
        - setWeight: 80
        - pause: {duration: 30s}
```

The `ScaledObject` targets the Rollout directly; the Kedify scaler resolves
`stableService` from the Rollout spec automatically:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: myapp
spec:
  scaleTargetRef:
    apiVersion: argoproj.io/v1alpha1
    kind: Rollout
    name: myapp
  triggers:
    - type: kedify-http
      metadata:
        hosts: myapp.example.com
        port: "80"
        scalingMetric: requestRate
        targetValue: "5"
        trafficAutowire: "ingress"
```

## Worked example

A complete, runnable example (Rollout + Services + Ingress + ScaledObject + RBAC)
lives at
[`kedify/examples/samples/argo-rollouts-canary`](https://github.com/kedify/examples/tree/main/samples/argo-rollouts-canary).

## Local development

For iteration on a local cluster (k3d, kind, etc.) where you want to test
unreleased changes without publishing a release, build the binary and load
it into the controller pod yourself:

```sh
make build           # produces ./rollouts-plugin-kedify (linux/amd64, static)
make test
make lint
make docker-build    # multi-stage image: ghcr.io/kedify/argo-rollouts-plugin:dev
make k3d-import      # builds + imports the image into k3d
```

Then patch the `argo-rollouts` Deployment with an init container that copies
the binary out of the image into the existing `plugin-bin` `emptyDir` (run as
the same uid as the controller — `999` for the upstream image). Point the
ConfigMap `location` at the resulting `file://` path. This route exists
purely for fast local iteration; production installs should use the
`https://` location pointing at a release asset.

## Releases

Releases are produced by [GoReleaser](https://goreleaser.com/) on tag push
(`v*`). Each release includes one binary per supported `os/arch` plus a
`checksums.txt` with SHA-256 hashes — copy the relevant hash into your
`argo-rollouts-config` ConfigMap to pin the binary.

```sh
git tag v0.1.0
git push origin v0.1.0
```

The `release` workflow (`.github/workflows/release.yml`) takes care of the
rest.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
