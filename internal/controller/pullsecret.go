package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// requiredPullSecretKeys are the dockerconfig keys the grid's image-pull
// surface provably depends on (exercised end to end in the live demo). The
// path-scoped quay.io/rhoai entry is the subtle one: no single token pulls
// both openshift-release-dev and rhoai/* from quay.io, so the composition
// must carry both keys with different credentials, matched by
// longest-prefix.
var requiredPullSecretKeys = []string{
	"cloud.openshift.com",
	"quay.io",
	"quay.io/rhoai",
	"registry.redhat.io",
}

// optionalPullSecretKeys were present in the validated composition but no
// pull in the demo flow demonstrably required them. Their absence is not
// an error; a future image dependency can promote them to required.
var optionalPullSecretKeys = []string{
	"registry.connect.redhat.com",
	"registry.stage.redhat.io",
}

// Sentinel digests for the functional probe: the exact images whose pulls
// failed during the demo's pull-secret incidents. A successful manifest
// fetch through each key proves the credential works for the repositories
// the grid's boot and RHOAI installation depend on.
const (
	sentinelReleaseRepo   = "openshift-release-dev/ocp-v4.0-art-dev"
	sentinelReleaseDigest = "sha256:d334719810c9814aa6f7159c64219bac1f757c61db405547eda6d4ff49a54bac"

	sentinelRhoAIRepo   = "rhoai/odh-operator-bundle"
	sentinelRhoAIDigest = "sha256:1227e86e39ae4553bcf9b4bf986212a1da57d0dc0eb3b5f1557f693bd0675184"
)

// pullSecretError carries a condition reason so the phase machine reports
// pull-secret failures distinctly instead of a generic PhaseError.
type pullSecretError struct {
	reason  string
	message string
}

func (e *pullSecretError) Error() string { return e.message }

// dockerConfig is the subset of a .dockerconfigjson the validation reads.
type dockerConfig struct {
	Auths map[string]struct {
		Auth string `json:"auth"`
	} `json:"auths"`
}

// validatePullSecret checks the grid's pull secret in two layers:
//
//  1. Structural: every required registry key is present. Catches
//     composition mistakes (a missing path-scoped quay.io/rhoai key, or
//     the wrong secret referenced entirely) before any LWE is created.
//  2. Functional: the quay.io and quay.io/rhoai credentials can fetch
//     their sentinel manifests. Catches expired or revoked tokens and
//     right-key-wrong-credential mistakes, which structure alone cannot
//     see.
//
// Both failure classes are retried on the normal phase cadence: fixing the
// secret self-heals the grid without operator intervention.
func (r *AIGridReconciler) validatePullSecret(ctx context.Context, grid *aigridv1alpha1.AIGrid) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: grid.Namespace, Name: grid.Spec.PullSecretRef.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return &pullSecretError{
				reason:  "PullSecretInvalid",
				message: fmt.Sprintf("pull secret %s not found in %s", grid.Spec.PullSecretRef.Name, grid.Namespace),
			}
		}
		return err
	}

	config, err := parseDockerConfig(secret.Data[".dockerconfigjson"])
	if err != nil {
		return &pullSecretError{reason: "PullSecretInvalid", message: fmt.Sprintf("parsing %s: %v", grid.Spec.PullSecretRef.Name, err)}
	}

	if missing := missingPullSecretKeys(config, requiredPullSecretKeys); len(missing) > 0 {
		return &pullSecretError{
			reason:  "PullSecretInvalid",
			message: fmt.Sprintf("pull secret %s is missing required registry keys: %s (required: %s)", grid.Spec.PullSecretRef.Name, strings.Join(missing, ", "), strings.Join(requiredPullSecretKeys, ", ")),
		}
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}

	if auth := config.Auths["quay.io"].Auth; auth != "" {
		if err := probeQuayManifest(httpClient, auth, sentinelReleaseRepo, sentinelReleaseDigest); err != nil {
			return &pullSecretError{
				reason:  "PullSecretUnauthorized",
				message: fmt.Sprintf("quay.io credential cannot pull %s: %v", sentinelReleaseRepo, err),
			}
		}
	}

	if auth := config.Auths["quay.io/rhoai"].Auth; auth != "" {
		if err := probeQuayManifest(httpClient, auth, sentinelRhoAIRepo, sentinelRhoAIDigest); err != nil {
			return &pullSecretError{
				reason:  "PullSecretUnauthorized",
				message: fmt.Sprintf("quay.io/rhoai credential cannot pull %s: %v", sentinelRhoAIRepo, err),
			}
		}
	}

	return nil
}

func parseDockerConfig(raw []byte) (*dockerConfig, error) {
	config := &dockerConfig{}
	if err := json.Unmarshal(raw, config); err != nil {
		return nil, err
	}
	if config.Auths == nil {
		return nil, fmt.Errorf("no auths in dockerconfigjson")
	}
	return config, nil
}

func missingPullSecretKeys(config *dockerConfig, required []string) []string {
	var missing []string
	for _, key := range required {
		if _, ok := config.Auths[key]; !ok {
			missing = append(missing, key)
		}
	}
	return missing
}

// probeQuayManifest fetches a manifest by digest through a dockerconfig
// auth (base64 user:pass), following the quay.io token exchange. A token
// alone proves nothing — quay issues tokens regardless of repository
// access — so the manifest fetch is the actual authorization check.
func probeQuayManifest(httpClient *http.Client, auth, repository, digest string) error {
	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return fmt.Errorf("decoding auth: %w", err)
	}
	credentials := strings.SplitN(string(decoded), ":", 2)
	if len(credentials) != 2 {
		return fmt.Errorf("auth is not user:pass")
	}

	tokenURL := fmt.Sprintf("https://quay.io/v2/auth?service=quay.io&scope=repository:%s:pull", url.QueryEscape(repository))
	tokenRequest, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tokenURL, nil)
	if err != nil {
		return err
	}
	tokenRequest.SetBasicAuth(credentials[0], credentials[1])
	tokenResponse, err := httpClient.Do(tokenRequest)
	if err != nil {
		return err
	}
	defer tokenResponse.Body.Close()
	tokenBody, _ := io.ReadAll(io.LimitReader(tokenResponse.Body, 1<<16))
	if tokenResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("token exchange returned %d", tokenResponse.StatusCode)
	}
	token := struct {
		Token string `json:"token"`
	}{}
	if err := json.Unmarshal(tokenBody, &token); err != nil || token.Token == "" {
		return fmt.Errorf("token exchange returned no token")
	}

	manifestURL := fmt.Sprintf("https://quay.io/v2/%s/manifests/%s", repository, digest)
	manifestRequest, err := http.NewRequestWithContext(context.Background(), http.MethodGet, manifestURL, nil)
	if err != nil {
		return err
	}
	manifestRequest.Header.Set("Authorization", "Bearer "+token.Token)
	manifestRequest.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json")
	manifestResponse, err := httpClient.Do(manifestRequest)
	if err != nil {
		return err
	}
	defer manifestResponse.Body.Close()
	io.Copy(io.Discard, io.LimitReader(manifestResponse.Body, 1<<16))
	if manifestResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("manifest fetch returned %d", manifestResponse.StatusCode)
	}
	return nil
}
