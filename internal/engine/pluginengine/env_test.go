package pluginengine

import (
	"strings"
	"testing"
)

func TestPluginEnvironmentRemovesAttemptCredentials(t *testing.T) {
	t.Setenv("PATCHPROOF_ATTEMPT_TOKEN", "attempt-secret")
	t.Setenv("PATCHPROOF_CALLBACK_URL", "https://controller.example/callback")
	t.Setenv("PATCHPROOF_S3_ACCESS_KEY", "access-key")
	t.Setenv("PATCHPROOF_CAS_DRIVER", "s3")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("PATCH_TOOLCHAIN", "keep-me")

	env := pluginEnvironment()
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{
		"PATCHPROOF_ATTEMPT_TOKEN", "PATCHPROOF_CALLBACK_URL",
		"PATCHPROOF_S3_ACCESS_KEY", "PATCHPROOF_CAS_DRIVER",
		"AWS_SECRET_ACCESS_KEY",
	} {
		if strings.Contains(joined, forbidden+"=") {
			t.Fatalf("plugin environment leaked %s: %v", forbidden, env)
		}
	}
	if !strings.Contains(joined, "PATCH_TOOLCHAIN=keep-me") {
		t.Fatalf("plugin environment dropped a non-sensitive variable: %v", env)
	}
}

func TestPluginEnvForbiddenDoesNotRejectOrdinaryVariables(t *testing.T) {
	for _, entry := range []string{"PATH=/usr/bin", "HOME=/tmp", "LANG=C", "GOCACHE=/tmp/cache"} {
		if pluginEnvForbidden(entry) {
			t.Fatalf("%q was incorrectly filtered", entry)
		}
	}
	for _, entry := range []string{"MALFORMED", "=value", "AWS_REGION=us-east-1"} {
		if !pluginEnvForbidden(entry) {
			t.Fatalf("%q was not filtered", entry)
		}
	}
}
