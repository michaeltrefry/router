package auth_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"weave-os/router/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type adminInstallationRepo struct {
	*fakeInstallationRepository
	installations []*auth.Installation
}

func (r adminInstallationRepo) ListForExternalID(_ context.Context, externalID string) ([]*auth.Installation, error) {
	var out []*auth.Installation
	for _, installation := range r.installations {
		if installation.ExternalID == externalID {
			out = append(out, installation)
		}
	}
	return out, nil
}

// fakePersonalKeyStore keeps subjects by email the way the identity table does.
type fakePersonalKeyStore struct {
	subjects map[string]*auth.PersonalSubject
	created  []auth.CreateAPIKeyParams
	rotated  []string
	reissued []string
	seq      int
}

func (f *fakePersonalKeyStore) FindPersonalSubject(_ context.Context, installationID, email string) (*auth.PersonalSubject, error) {
	subject, ok := f.subjects[installationID+"/"+email]
	if !ok {
		return nil, nil
	}
	return subject, nil
}

func (f *fakePersonalKeyStore) key(subjectID string, params auth.CreateAPIKeyParams) *auth.APIKey {
	f.seq++
	id := fmt.Sprintf("key-%d", f.seq)
	return &auth.APIKey{ID: id, InstallationID: params.InstallationID, CredentialSubjectID: subjectID, Name: params.Name, KeyHash: params.KeyHash, KeyPrefix: params.KeyPrefix, KeySuffix: params.KeySuffix, Scope: params.Scope}
}

func (f *fakePersonalKeyStore) CreateSelfHostedPersonal(_ context.Context, _ string, email string, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	f.created = append(f.created, params)
	key := f.key("subject-"+email, params)
	if f.subjects == nil {
		f.subjects = map[string]*auth.PersonalSubject{}
	}
	f.subjects[params.InstallationID+"/"+email] = &auth.PersonalSubject{ID: key.CredentialSubjectID, ActiveKeyIDs: []string{key.ID}}
	return key, nil
}

func (f *fakePersonalKeyStore) Rotate(_ context.Context, _ string, subjectID, previousKeyID string, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	f.rotated = append(f.rotated, previousKeyID)
	return f.key(subjectID, params), nil
}

func (f *fakePersonalKeyStore) IssueForSubject(_ context.Context, _ string, subjectID string, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	f.reissued = append(f.reissued, subjectID)
	return f.key(subjectID, params), nil
}

func newPersonalKeyService(store auth.PersonalKeyStore, installations ...*auth.Installation) *auth.Service {
	repo := adminInstallationRepo{fakeInstallationRepository: &fakeInstallationRepository{}, installations: installations}
	return auth.NewService(repo, &fakeAPIKeyRepository{}, nil, nil, auth.NoOpAPIKeyCache{}, nil, time.Now).WithPersonalKeyStore(store)
}

var selfHostedAdmin = &auth.Installation{ID: "admin-installation", ExternalID: auth.AdminInstallationExternalID, Name: auth.AdminInstallationName}

func TestIssueSelfHostedPersonalKeyCreatesRoutingKeyBoundToNewSubject(t *testing.T) {
	store := &fakePersonalKeyStore{}
	svc := newPersonalKeyService(store, selfHostedAdmin)

	issued, err := svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: "  Operator@Example.TEST "})

	require.NoError(t, err)
	require.False(t, issued.Rotated)
	assert.Equal(t, selfHostedAdmin.ID, issued.Installation.ID)
	assert.True(t, strings.HasPrefix(issued.RawToken, auth.APIKeyPrefix), "personal keys are routing keys")
	require.Len(t, store.created, 1)
	created := store.created[0]
	assert.Equal(t, auth.ScopeRouting, created.Scope)
	assert.Equal(t, selfHostedAdmin.ID, created.InstallationID)
	hash, prefix, suffix := auth.APITokenFingerprint(issued.RawToken)
	assert.Equal(t, hash, created.KeyHash, "the stored hash must authenticate the printed token")
	assert.Equal(t, prefix, created.KeyPrefix)
	assert.Equal(t, suffix, created.KeySuffix)
	assert.NotContains(t, created.KeyHash, issued.RawToken)
	require.NotNil(t, created.Name)
	assert.Contains(t, *created.Name, "operator@example.test", "email is normalized before it names the key")
	assert.Equal(t, "subject-operator@example.test", issued.Key.CredentialSubjectID)
}

func TestIssueSelfHostedPersonalKeyRefusesRerunWithoutRotate(t *testing.T) {
	store := &fakePersonalKeyStore{}
	svc := newPersonalKeyService(store, selfHostedAdmin)
	_, err := svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: "operator@example.test"})
	require.NoError(t, err)

	_, err = svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: "OPERATOR@example.test"})

	require.ErrorIs(t, err, auth.ErrPersonalKeyExists)
	assert.Len(t, store.created, 1, "a re-run must not create an orphan subject")
	assert.Empty(t, store.rotated)
}

func TestIssueSelfHostedPersonalKeyRotatesTheSubjectsOnlyKey(t *testing.T) {
	store := &fakePersonalKeyStore{}
	svc := newPersonalKeyService(store, selfHostedAdmin)
	first, err := svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: "operator@example.test"})
	require.NoError(t, err)

	rotated, err := svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: "operator@example.test", Rotate: true})

	require.NoError(t, err)
	require.True(t, rotated.Rotated)
	assert.Equal(t, []string{first.Key.ID}, store.rotated)
	assert.Equal(t, first.Key.CredentialSubjectID, rotated.Key.CredentialSubjectID, "subscriptions stay with the subject across rotation")
	assert.NotEqual(t, first.RawToken, rotated.RawToken)
	assert.Len(t, store.created, 1)
}

func TestIssueSelfHostedPersonalKeyReissuesWhenSubjectKeyWasDeleted(t *testing.T) {
	store := &fakePersonalKeyStore{subjects: map[string]*auth.PersonalSubject{
		selfHostedAdmin.ID + "/operator@example.test": {ID: "subject-kept"},
	}}
	svc := newPersonalKeyService(store, selfHostedAdmin)

	issued, err := svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: "operator@example.test", Rotate: true})

	require.NoError(t, err)
	assert.Equal(t, []string{"subject-kept"}, store.reissued)
	assert.Equal(t, "subject-kept", issued.Key.CredentialSubjectID)
	assert.Empty(t, store.created)
}

func TestIssueSelfHostedPersonalKeyRefusesAmbiguousRotation(t *testing.T) {
	store := &fakePersonalKeyStore{subjects: map[string]*auth.PersonalSubject{
		selfHostedAdmin.ID + "/operator@example.test": {ID: "subject-kept", ActiveKeyIDs: []string{"a", "b"}},
	}}
	svc := newPersonalKeyService(store, selfHostedAdmin)

	_, err := svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: "operator@example.test", Rotate: true})

	require.ErrorIs(t, err, auth.ErrPersonalKeyAmbiguous)
	assert.Empty(t, store.rotated)
	assert.Empty(t, store.reissued)
}

func TestIssueSelfHostedPersonalKeyRequiresSelfHostedAdminInstallation(t *testing.T) {
	store := &fakePersonalKeyStore{}
	managedOnly := &auth.Installation{ID: "org-installation", ExternalID: "org-external"}
	svc := newPersonalKeyService(store, managedOnly)

	_, err := svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: "operator@example.test"})

	require.ErrorIs(t, err, auth.ErrAdminInstallationMissing)
	assert.Empty(t, store.created)
}

func TestIssueSelfHostedPersonalKeyRejectsInvalidEmail(t *testing.T) {
	for _, email := range []string{"", "not-an-email", "Operator <operator@example.test>", "a@b@c"} {
		store := &fakePersonalKeyStore{}
		svc := newPersonalKeyService(store, selfHostedAdmin)

		_, err := svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: email})

		require.ErrorIs(t, err, auth.ErrInvalidPersonalKeyEmail, email)
		assert.Empty(t, store.created)
	}
}

func TestIssueSelfHostedPersonalKeyRequiresStore(t *testing.T) {
	svc := auth.NewService(adminInstallationRepo{fakeInstallationRepository: &fakeInstallationRepository{}, installations: []*auth.Installation{selfHostedAdmin}}, &fakeAPIKeyRepository{}, nil, nil, auth.NoOpAPIKeyCache{}, nil, time.Now)

	_, err := svc.IssueSelfHostedPersonalKey(context.Background(), auth.IssuePersonalKeyParams{Email: "operator@example.test"})

	require.Error(t, err)
	assert.False(t, errors.Is(err, auth.ErrPersonalKeyExists))
}
