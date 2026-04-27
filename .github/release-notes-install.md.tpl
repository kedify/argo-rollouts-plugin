## Install

Add this entry to the `argo-rollouts-config` ConfigMap in the `argo-rollouts` namespace and restart the argo-rollouts controller:

```yaml
data:
  trafficRouterPlugins: |
    - name: "kedify/http"
      location: "${BASE_URL}/rollouts-plugin-kedify-linux-amd64"
      sha256: "${SHA_LINUX_AMD64}"
```

<details>

<summary>for linux arm64</summary>

```yaml
data:
  trafficRouterPlugins: |
    - name: "kedify/http"
      location: "${BASE_URL}/rollouts-plugin-kedify-linux-arm64"
      sha256: "${SHA_LINUX_ARM64}"
```

</details>

<details>

<summary>for darwin amd64</summary>

```yaml
data:
  trafficRouterPlugins: |
    - name: "kedify/http"
      location: "${BASE_URL}/rollouts-plugin-kedify-darwin-amd64"
      sha256: "${SHA_DARWIN_AMD64}"
```

</details>

<details>

<summary>for darwin arm64</summary>

```yaml
data:
  trafficRouterPlugins: |
    - name: "kedify/http"
      location: "${BASE_URL}/rollouts-plugin-kedify-darwin-arm64"
      sha256: "${SHA_DARWIN_ARM64}"
```

</details>

See the [README](${REPO_URL}#install) for the required RBAC and a full example.
