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

Opt-in cookie session persistence uses one Envoy cluster containing weighted
stable and canary endpoints. Envoy's native session filter keeps subsequent
requests on the assigned endpoint. See [Session persistence](#session-persistence).

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

When `setWeight` reaches `0` the plugin removes its routing annotations and the
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

## Session persistence

This option requires plugin and HTTP add-on builds containing session persistence
support, plus a KEDA HTTP scaler that preserves the plugin's HSO annotations.
Older releases that only generate `weighted_clusters` do not support it.
The proxy must include Envoy's native stateful-session cookie filter; the feature
is validated with Envoy `1.39.3`.

Add the following to the plugin configuration in the Rollout:

```yaml
trafficRouting:
  plugins:
    kedify/http:
      httpScaledObjectName: myapp
      sessionPersistence:
        type: Cookie
        absoluteTimeout: 30m
        cookie:
          name: rollout-session
```

`cookie.name` is an optional prefix, defaulting to `kedify-rollout`. The plugin
appends an identifier scoped to the namespace, Rollout identity, HSO and current
canary revision. The name stays unchanged as weights progress, and a new revision
gets a different name. `absoluteTimeout` defaults to `30m` and must be a whole
number of seconds, at least `1s`. Cookies use `Path=/`, `HttpOnly` and
`SameSite=Lax`; clients must return the cookie on subsequent requests.

With this option, the HTTP add-on uses one cluster with weighted endpoints and
Envoy's native stateful-session filter. New sessions follow the rollout weights.
Existing sessions retain their endpoint across weight changes and scale-out, so
the weights describe new assignments rather than total request percentages.

- **DNS discovery:** stable and canary must be ordinary ClusterIP Services. The
  cookie pins the resolved Service IP and port; pod replacement behind that
  Service does not change the selected version. Headless Services require EDS.
- **EDS discovery:** the add-on divides each version's weight among its ready
  endpoints and recalculates it as pods change. If a pinned pod disappears,
  Envoy chooses another endpoint using the current weights, possibly from the
  other version, and issues a replacement cookie. Requests then stay on that
  endpoint while it remains available. Unready endpoint groups receive no traffic.
- **Cold starts:** requests still queue at the interceptor. After readiness, the
  interceptor selects an application endpoint with the same weights and issues
  a cookie compatible with the warm Envoy path. Waiting pages and timeout
  fallback responses do not establish application affinity.
- **Completion and abort:** weight zero and managed-route cleanup remove both
  annotations. Requests return to the primary stable Service and old cookies are
  ignored. A backend at zero weight is removed from the sticky endpoint pool,
  so its existing sessions are reassigned.

The sticky cluster requires `ROUND_ROBIN` without a custom or subset load-balancing
policy. TLS passthrough and inference pools are unsupported. This feature does not
keep application state alive after pod removal or retain old static assets after
promotion. Keep assets referenced by already-open pages available across versions
if those pages must survive a version change.

Omitting `sessionPersistence` preserves the existing weighted-cluster behavior.
Removing the option during a rollout clears its session-persistence annotation.

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
