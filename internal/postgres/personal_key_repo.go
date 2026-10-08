package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/postgres/dbbudget"
	"weave-os/router/internal/sqlc"
)

// FindPersonalSubject resolves a live email identity to its subject and active keys; nil when absent.
func (r *CredentialSubjectRepo) FindPersonalSubject(ctx context.Context, installationID, email string) (*auth.PersonalSubject, error) {
	installationUUID, err := uuid.Parse(installationID)
	if err != nil {
		return nil, err
	}
	queries := dbbudget.Queries(r.pool)
	subjectID, err := queries.GetCredentialSubjectIdentityByEmail(ctx, sqlc.GetCredentialSubjectIdentityByEmailParams{InstallationID: installationUUID, Email: email})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keyIDs, err := queries.GetActivePersonalKeyIDsForSubject(ctx, sqlc.GetActivePersonalKeyIDsForSubjectParams{SubjectID: subjectID, InstallationID: installationUUID})
	if err != nil {
		return nil, err
	}
	subject := &auth.PersonalSubject{ID: subjectID.String(), ActiveKeyIDs: make([]string, 0, len(keyIDs))}
	for _, id := range keyIDs {
		subject.ActiveKeyIDs = append(subject.ActiveKeyIDs, id.String())
	}
	return subject, nil
}

// CreateSelfHostedPersonal issues a subject, its email identity and first key in
// one transaction, with the ownership projection complete and access enabled.
// Self-hosted has no control plane to complete it later; the operator who runs
// the command is the account owner. No profile assignment is written: that is
// installation-wide and only managed serving admission reads it.
func (r *CredentialSubjectRepo) CreateSelfHostedPersonal(ctx context.Context, externalID, email string, key auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	installationID, err := uuid.Parse(key.InstallationID)
	if err != nil {
		return nil, err
	}
	if key.Scope.Normalized() != auth.ScopeRouting {
		return nil, auth.ErrInvalidKeyScope
	}
	var created *auth.APIKey
	err = pgx.BeginFunc(ctx, dbbudget.NewDBTX(r.pool), func(tx pgx.Tx) error {
		queries := dbbudget.Queries(tx)
		if err := lockInstallation(ctx, queries, externalID, installationID); err != nil {
			return err
		}
		var subjectID uuid.UUID
		created, subjectID, err = insertPendingPersonal(ctx, queries, installationID, key)
		if err != nil {
			return err
		}
		if err := queries.InsertCredentialSubjectIdentity(ctx, sqlc.InsertCredentialSubjectIdentityParams{SubjectID: subjectID, InstallationID: installationID, Email: email}); err != nil {
			var constraint *pgconn.PgError
			if errors.As(err, &constraint) && constraint.Code == uniqueViolationCode {
				return auth.ErrPersonalKeyExists
			}
			return err
		}
		if changed, err := queries.UpdateCredentialSubjectProjection(ctx, subjectID); err != nil || changed != 1 {
			return errors.Join(err, errors.New("credential subject projection was not completed"))
		}
		if changed, err := queries.UpdateCredentialSubjectInstallationAccess(ctx, sqlc.UpdateCredentialSubjectInstallationAccessParams{SubjectID: subjectID, InstallationID: installationID, AccessEnabled: true}); err != nil || changed != 1 {
			return errors.Join(err, errors.New("credential subject access was not enabled"))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create self-hosted personal credential: %w", err)
	}
	return created, nil
}

// IssueForSubject adds a key to a live, fully projected subject that has none left.
func (r *CredentialSubjectRepo) IssueForSubject(ctx context.Context, externalID, subjectID string, key auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	installationID, err := uuid.Parse(key.InstallationID)
	if err != nil {
		return nil, err
	}
	subjectUUID, err := uuid.Parse(subjectID)
	if err != nil {
		return nil, err
	}
	if key.Scope.Normalized() != auth.ScopeRouting {
		return nil, auth.ErrInvalidKeyScope
	}
	var created *auth.APIKey
	err = pgx.BeginFunc(ctx, dbbudget.NewDBTX(r.pool), func(tx pgx.Tx) error {
		queries := dbbudget.Queries(tx)
		if err := lockInstallation(ctx, queries, externalID, installationID); err != nil {
			return err
		}
		projection, err := queries.GetServingSubjectForAdmission(ctx, sqlc.GetServingSubjectForAdmissionParams{SubjectID: subjectUUID, InstallationID: installationID})
		if err != nil {
			return err
		}
		if !projection.RouterCredentialSubject.ProjectionComplete || projection.RouterCredentialSubject.RevokedAt.Valid || !projection.AccessEnabled {
			return auth.ErrPersonalCredentialRequired
		}
		created, err = insertPersonalKey(ctx, queries, installationID, subjectUUID, key)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reissue personal credential: %w", err)
	}
	return created, nil
}

var _ auth.PersonalKeyStore = (*CredentialSubjectRepo)(nil)
