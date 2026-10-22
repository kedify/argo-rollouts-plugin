package plugin

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
)

const sessionPersistenceAnnotation = "http.kedify.io/session-persistence"

// SessionPersistence opts a rollout into cookie-based backend persistence.
type SessionPersistence struct {
	Type            string        `json:"type"`
	Cookie          SessionCookie `json:"cookie,omitempty"`
	AbsoluteTimeout string        `json:"absoluteTimeout,omitempty"`
}

// SessionCookie configures the cookie name prefix.
type SessionCookie struct {
	// Name is a prefix; the plugin appends an application/revision identifier.
	Name string `json:"name,omitempty"`
}

func sessionAnnotation(rollout *v1alpha1.Rollout, cfg *KedifyPluginConfig) (string, error) {
	if cfg.SessionPersistence == nil {
		return "", nil
	}
	session := *cfg.SessionPersistence
	if session.Type != "Cookie" {
		return "", fmt.Errorf("sessionPersistence.type must be Cookie")
	}
	if session.Cookie.Name == "" {
		session.Cookie.Name = "kedify-rollout"
	}
	if len(session.Cookie.Name) > 200 {
		return "", fmt.Errorf("sessionPersistence.cookie.name must not exceed 200 characters")
	}
	if err := (&http.Cookie{Name: session.Cookie.Name}).Valid(); err != nil {
		return "", fmt.Errorf("invalid sessionPersistence.cookie.name: %w", err)
	}
	if session.AbsoluteTimeout == "" {
		session.AbsoluteTimeout = "30m"
	}
	ttl, err := time.ParseDuration(session.AbsoluteTimeout)
	if err != nil || ttl < time.Second || ttl%time.Second != 0 {
		return "", fmt.Errorf("sessionPersistence.absoluteTimeout must be a whole number of seconds, at least 1s")
	}
	// The controller updates the revision annotation before traffic reconciliation;
	// status.currentPodHash can still describe the previous revision at this point.
	revision, err := strconv.ParseUint(rollout.Annotations["rollout.argoproj.io/revision"], 10, 64)
	if err != nil || revision == 0 {
		return "", fmt.Errorf("session persistence requires the rollout.argoproj.io/revision annotation")
	}
	scope := fmt.Sprintf("%s/%s/%s/%s/%d", rollout.Namespace, rollout.Name, rollout.UID, cfg.HTTPScaledObjectName, revision)
	hash := sha256.Sum256([]byte(scope))
	session.Cookie.Name = fmt.Sprintf("%s-%x", session.Cookie.Name, hash[:8])
	encoded, err := json.Marshal(session)
	if err != nil {
		return "", fmt.Errorf("marshal session persistence: %w", err)
	}
	return string(encoded), nil
}
