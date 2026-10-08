package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/auth"
)

type fakeIssuer struct {
	params []auth.IssuePersonalKeyParams
	issue  *auth.PersonalKeyIssue
	err    error
}

func (f *fakeIssuer) IssueSelfHostedPersonalKey(_ context.Context, params auth.IssuePersonalKeyParams) (*auth.PersonalKeyIssue, error) {
	f.params = append(f.params, params)
	return f.issue, f.err
}

func runWith(t *testing.T, fake *fakeIssuer, env map[string]string, args ...string) (int, string, string, bool) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opened := false
	code := run(context.Background(), args, func(key string) string { return env[key] }, &stdout, &stderr, func(context.Context) (issuer, func(), error) {
		opened = true
		return fake, func() {}, nil
	})
	return code, stdout.String(), stderr.String(), opened
}

func TestRunPrintsRawKeyOnceToStdout(t *testing.T) {
	fake := &fakeIssuer{issue: &auth.PersonalKeyIssue{
		Key:          &auth.APIKey{ID: "key-1", KeySuffix: "wxyz", CredentialSubjectID: "subject-1"},
		RawToken:     "rk_synthetic_personal_token",
		Installation: &auth.Installation{ID: "installation-1"},
	}}

	code, stdout, stderr, _ := runWith(t, fake, nil, "-email", "operator@example.test")

	require.Equal(t, 0, code, stderr)
	assert.Equal(t, []auth.IssuePersonalKeyParams{{Email: "operator@example.test"}}, fake.params)
	assert.Contains(t, stdout, "Personal router key (shown once")
	assert.Equal(t, 1, strings.Count(stdout, "rk_synthetic_personal_token"))
	assert.Contains(t, stdout, "login claude --local")
	assert.Contains(t, stdout, "login codex --local")
	assert.NotContains(t, stderr, "rk_synthetic_personal_token")
}

func TestRunPassesRotate(t *testing.T) {
	fake := &fakeIssuer{issue: &auth.PersonalKeyIssue{Key: &auth.APIKey{ID: "key-2"}, RawToken: "rk_rotated", Installation: &auth.Installation{ID: "i"}, Rotated: true}}

	code, stdout, _, _ := runWith(t, fake, map[string]string{"ROUTER_DEPLOYMENT_MODE": "selfhosted"}, "-email", "operator@example.test", "-rotate")

	require.Equal(t, 0, code)
	assert.True(t, fake.params[0].Rotate)
	assert.True(t, strings.HasPrefix(stdout, "Rotated personal router key key-2"))
}

func TestRunRefusesManagedMode(t *testing.T) {
	fake := &fakeIssuer{}

	code, _, stderr, opened := runWith(t, fake, map[string]string{"ROUTER_DEPLOYMENT_MODE": "managed"}, "-email", "operator@example.test")

	assert.Equal(t, 1, code)
	assert.False(t, opened, "managed mode must refuse before touching the database")
	assert.Contains(t, stderr, "self-hosted")
}

func TestRunExplainsRerunWithoutRotate(t *testing.T) {
	fake := &fakeIssuer{err: auth.ErrPersonalKeyExists}

	code, stdout, stderr, _ := runWith(t, fake, nil, "-email", "operator@example.test")

	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "ROTATE=1")
}

func TestRunRequiresEmail(t *testing.T) {
	code, _, stderr, opened := runWith(t, &fakeIssuer{}, nil)

	assert.Equal(t, 2, code)
	assert.False(t, opened)
	assert.Contains(t, stderr, "-email is required")
}
