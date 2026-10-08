package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

var (
	// ErrPersonalKeyExists refuses a second subject for an email that already has one.
	ErrPersonalKeyExists = errors.New("a personal router key already exists for this email; rotate it instead")
	// ErrPersonalKeyAmbiguous refuses to guess which of several live keys a rotation replaces.
	ErrPersonalKeyAmbiguous = errors.New("this email's subject has more than one active key; delete the extras before rotating")
	// ErrInvalidPersonalKeyEmail rejects anything but a bare address.
	ErrInvalidPersonalKeyEmail = errors.New("personal router keys need a bare email address")
	// ErrAdminInstallationMissing means the database holds no self-hosted admin installation.
	ErrAdminInstallationMissing = errors.New("no self-hosted admin installation found")
)

// PersonalSubject is the credential subject an email already maps to, with its live key IDs.
type PersonalSubject struct {
	ID           string
	ActiveKeyIDs []string
}

// PersonalKeyStore persists self-hosted personal routing keys. Each write is
// atomic, so a failure never leaves a subject without its key or identity.
type PersonalKeyStore interface {
	// FindPersonalSubject returns nil when the email maps to no live subject.
	FindPersonalSubject(ctx context.Context, installationID, email string) (*PersonalSubject, error)
	// CreateSelfHostedPersonal creates the subject, its email identity and its
	// first key with the ownership projection already complete and enabled.
	CreateSelfHostedPersonal(ctx context.Context, externalID, email string, key CreateAPIKeyParams) (*APIKey, error)
	// Rotate replaces previousKeyID, keeping the subject and its subscriptions.
	Rotate(ctx context.Context, externalID, subjectID, previousKeyID string, replacement CreateAPIKeyParams) (*APIKey, error)
	// IssueForSubject adds a key to a live subject whose keys were all deleted.
	IssueForSubject(ctx context.Context, externalID, subjectID string, key CreateAPIKeyParams) (*APIKey, error)
}

// IssuePersonalKeyParams names the operator; Rotate replaces an existing key instead of refusing.
type IssuePersonalKeyParams struct {
	Email  string
	Rotate bool
}

// PersonalKeyIssue carries the raw token, which exists only here and is never stored.
type PersonalKeyIssue struct {
	Key          *APIKey
	RawToken     string
	Installation *Installation
	Rotated      bool
}

// WithPersonalKeyStore attaches personal-key persistence for the self-hosted issuance command.
func (s *Service) WithPersonalKeyStore(store PersonalKeyStore) *Service {
	s.personalKeys = store
	return s
}

// NormalizePersonalKeyEmail trims and lower-cases a bare address, the form the identity table stores.
func NormalizePersonalKeyEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || parsed.Name != "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidPersonalKeyEmail, raw)
	}
	return email, nil
}

// IssueSelfHostedPersonalKey issues a personal routing key on the self-hosted
// admin installation. Without a control plane the operator is the account
// owner, so the subject is projected complete and enabled at creation. A
// re-run for the same email refuses unless Rotate is set.
func (s *Service) IssueSelfHostedPersonalKey(ctx context.Context, params IssuePersonalKeyParams) (*PersonalKeyIssue, error) {
	if s.personalKeys == nil {
		return nil, errors.New("personal key store is not configured")
	}
	email, err := NormalizePersonalKeyEmail(params.Email)
	if err != nil {
		return nil, err
	}
	installation, found, err := s.findAdminInstallation(ctx)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrAdminInstallationMissing
	}
	existing, err := s.personalKeys.FindPersonalSubject(ctx, installation.ID, email)
	if err != nil {
		return nil, err
	}
	if existing != nil && !params.Rotate {
		return nil, ErrPersonalKeyExists
	}
	if existing != nil && len(existing.ActiveKeyIDs) > 1 {
		return nil, ErrPersonalKeyAmbiguous
	}

	rawToken := GenerateID(APIKeyPrefix)
	keyHash, keyPrefix, keySuffix := APITokenFingerprint(rawToken)
	name := "Personal key (" + email + ")"
	keyParams := CreateAPIKeyParams{
		InstallationID: installation.ID,
		ExternalID:     GenerateID("kid"),
		Name:           &name,
		KeyPrefix:      keyPrefix,
		KeyHash:        keyHash,
		KeySuffix:      keySuffix,
		Scope:          ScopeRouting,
	}

	var key *APIKey
	switch {
	case existing == nil:
		key, err = s.personalKeys.CreateSelfHostedPersonal(ctx, installation.ExternalID, email, keyParams)
	case len(existing.ActiveKeyIDs) == 1:
		key, err = s.personalKeys.Rotate(ctx, installation.ExternalID, existing.ID, existing.ActiveKeyIDs[0], keyParams)
	default:
		key, err = s.personalKeys.IssueForSubject(ctx, installation.ExternalID, existing.ID, keyParams)
	}
	if err != nil {
		return nil, err
	}
	return &PersonalKeyIssue{Key: key, RawToken: rawToken, Installation: installation, Rotated: existing != nil}, nil
}
