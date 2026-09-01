package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/integrations"
	"github.com/tokayops/tokayops/internal/model"
)

var (
	ErrIntegrationNotFound  = errors.New("integration not found")
	ErrDuplicateIntegration = errors.New("integration of this type already exists")

	// ErrIntegrationTeamNotFound means a team-scoped integration named a team
	// that does not exist by the time the row is written.
	//
	// The caller usually validated the team a moment earlier, so this is the
	// narrow race: the team was deleted between that check and this write.
	// Deleting a team takes a row lock precisely so its own outcome is
	// deterministic, which means the write on the other side is the one that
	// loses - and it has to lose in the vocabulary of the contract rather than
	// with the name of a foreign key.
	ErrIntegrationTeamNotFound = errors.New("integration team not found")
)

// CreateIntegration creates a new integration with encrypted config
func (s *Store) CreateIntegration(i *model.Integration) error {
	if i.ID == "" {
		i.ID = uuid.New().String()
	}
	i.CreatedAt = time.Now()
	i.UpdatedAt = time.Now()

	// Auto-set direction based on type. Type was validated at the API layer
	// (integrations.IsValidType); a missing descriptor here is an invariant
	// violation, not user error.
	dir, ok := integrations.DirectionFor(i.Type)
	if !ok {
		return fmt.Errorf("unknown integration type %s", i.Type)
	}
	i.Direction = dir

	// Encrypt config
	encryptedConfig, err := encryptConfig(i.Config)
	if err != nil {
		return err
	}

	query := `INSERT INTO integrations (id, type, direction, name, enabled, scope, team_id, config, created_at, updated_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	_, err = s.db.Exec(query, i.ID, i.Type, i.Direction, i.Name, i.Enabled, scopeToNullString(i.Scope), stringPtrToNullString(i.TeamID), encryptedConfig, i.CreatedAt, i.UpdatedAt)
	if err != nil {
		// Check for unique constraint violation (outbound duplicate)
		if isUniqueViolation(err) {
			return ErrDuplicateIntegration
		}
		if isIntegrationTeamFKViolation(err) {
			return ErrIntegrationTeamNotFound
		}
		return err
	}
	return nil
}

// GetIntegrationByID retrieves an integration by ID with decrypted config
func (s *Store) GetIntegrationByID(id string) (*model.Integration, error) {
	query := `SELECT id, type, direction, name, enabled, scope, team_id, config, created_at, updated_at FROM integrations WHERE id = $1`
	row := s.db.QueryRow(query, id)
	return scanIntegration(row)
}

// GetIntegrationByType retrieves the first enabled integration of a given type
func (s *Store) GetIntegrationByType(integrationType model.IntegrationType) (*model.Integration, error) {
	query := `SELECT id, type, direction, name, enabled, scope, team_id, config, created_at, updated_at
			  FROM integrations WHERE type = $1 AND enabled = true LIMIT 1`
	row := s.db.QueryRow(query, integrationType)
	return scanIntegration(row)
}

// GetIntegrationsByType retrieves all enabled integrations of a given type (for inbound)
func (s *Store) GetIntegrationsByType(integrationType model.IntegrationType) ([]*model.Integration, error) {
	query := `SELECT id, type, direction, name, enabled, scope, team_id, config, created_at, updated_at
			  FROM integrations WHERE type = $1 AND enabled = true`
	rows, err := s.db.Query(query, integrationType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIntegrations(rows)
}

// GetAllIntegrations retrieves all integrations with decrypted config
func (s *Store) GetAllIntegrations() ([]*model.Integration, error) {
	query := `SELECT id, type, direction, name, enabled, scope, team_id, config, created_at, updated_at FROM integrations ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIntegrations(rows)
}

// scanIntegration scans a single integration row
func scanIntegration(row *sql.Row) (*model.Integration, error) {
	var i model.Integration
	var encryptedConfig string
	var scopeNull, teamIDNull sql.NullString

	err := row.Scan(&i.ID, &i.Type, &i.Direction, &i.Name, &i.Enabled, &scopeNull, &teamIDNull, &encryptedConfig, &i.CreatedAt, &i.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrIntegrationNotFound
		}
		return nil, err
	}

	if scopeNull.Valid {
		s := model.WebhookScope(scopeNull.String)
		i.Scope = &s
	}
	if teamIDNull.Valid {
		i.TeamID = &teamIDNull.String
	}

	// Decrypt config
	decryptedConfig, err := decryptConfig(encryptedConfig)
	if err != nil {
		return nil, err
	}
	i.Config = decryptedConfig

	return &i, nil
}

// scanIntegrations scans multiple integration rows
func scanIntegrations(rows *sql.Rows) ([]*model.Integration, error) {
	var integrations []*model.Integration
	for rows.Next() {
		var i model.Integration
		var encryptedConfig string
		var scopeNull, teamIDNull sql.NullString

		if err := rows.Scan(&i.ID, &i.Type, &i.Direction, &i.Name, &i.Enabled, &scopeNull, &teamIDNull, &encryptedConfig, &i.CreatedAt, &i.UpdatedAt); err != nil {
			return nil, err
		}

		if scopeNull.Valid {
			s := model.WebhookScope(scopeNull.String)
			i.Scope = &s
		}
		if teamIDNull.Valid {
			i.TeamID = &teamIDNull.String
		}

		// Decrypt config
		decryptedConfig, err := decryptConfig(encryptedConfig)
		if err != nil {
			return nil, err
		}
		i.Config = decryptedConfig

		integrations = append(integrations, &i)
	}
	return integrations, nil
}

// encryptConfig encrypts JSON config and returns base64-encoded ciphertext
func encryptConfig(configJSON json.RawMessage) (string, error) {
	key, err := config.GetEncryptionKey()
	if err != nil {
		return "", err
	}

	ciphertext, err := config.Encrypt(configJSON, key)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decryptConfig decrypts base64-encoded ciphertext and returns JSON config
func decryptConfig(encryptedConfig string) (json.RawMessage, error) {
	key, err := config.GetEncryptionKey()
	if err != nil {
		return nil, err
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encryptedConfig)
	if err != nil {
		return nil, err
	}

	plaintext, err := config.Decrypt(ciphertext, key)
	if err != nil {
		return nil, err
	}

	return json.RawMessage(plaintext), nil
}

// mergeSecrets keeps existing secrets if new ones are empty or masked
func mergeSecrets(integrationType model.IntegrationType, existingConfig, newConfig json.RawMessage) json.RawMessage {
	switch integrationType {
	case model.IntegrationTypeSlack:
		var existing, new model.SlackConfig
		if err := json.Unmarshal(existingConfig, &existing); err != nil {
			return newConfig
		}
		if err := json.Unmarshal(newConfig, &new); err != nil {
			return newConfig
		}
		// Keep existing token if new is empty or masked
		if new.Token == "" || new.Token == model.MaskedSecret {
			new.Token = existing.Token
		}
		// Keep existing user_token if new is empty or masked
		if new.UserToken == "" || new.UserToken == model.MaskedSecret {
			new.UserToken = existing.UserToken
		}
		// Keep existing signing_secret if new is empty or masked
		if new.SigningSecret == "" || new.SigningSecret == model.MaskedSecret {
			new.SigningSecret = existing.SigningSecret
		}
		// Keep existing default_channel if not provided
		if new.DefaultChannel == "" {
			new.DefaultChannel = existing.DefaultChannel
		}
		merged, _ := json.Marshal(new)
		return merged

	case model.IntegrationTypeTelegram:
		var existing, new model.TelegramConfig
		if err := json.Unmarshal(existingConfig, &existing); err != nil {
			return newConfig
		}
		if err := json.Unmarshal(newConfig, &new); err != nil {
			return newConfig
		}
		// Keep existing bot_token if new is empty or masked
		if new.BotToken == "" || new.BotToken == model.MaskedSecret {
			new.BotToken = existing.BotToken
		}
		// Keep existing secret_token if new is empty or masked
		if new.SecretToken == "" || new.SecretToken == model.MaskedSecret {
			new.SecretToken = existing.SecretToken
		}
		// Keep existing default_chat_id if not provided
		if new.DefaultChatID == "" {
			new.DefaultChatID = existing.DefaultChatID
		}
		// Keep existing interactive if not provided. Without this an update that
		// omits the field would reset it to nil, which resolves to true, silently
		// switching the buttons back on for someone who had turned them off.
		if new.Interactive == nil {
			new.Interactive = existing.Interactive
		}
		merged, _ := json.Marshal(new)
		return merged

	case model.IntegrationTypeAlertmanagerWebhook:
		var existing, new model.WebhookConfig
		if err := json.Unmarshal(existingConfig, &existing); err != nil {
			return newConfig
		}
		if err := json.Unmarshal(newConfig, &new); err != nil {
			return newConfig
		}
		// Keep existing secret if new is empty or masked
		if new.Secret == "" || new.Secret == model.MaskedSecret {
			new.Secret = existing.Secret
		}
		merged, _ := json.Marshal(new)
		return merged

	case model.IntegrationTypeGenericWebhook:
		var existing, new model.GenericWebhookConfig
		if err := json.Unmarshal(existingConfig, &existing); err != nil {
			return newConfig
		}
		if err := json.Unmarshal(newConfig, &new); err != nil {
			return newConfig
		}
		if new.Secret == "" || new.Secret == model.MaskedSecret {
			new.Secret = existing.Secret
		}
		if new.URL == "" {
			new.URL = existing.URL
		}
		if new.TimeoutSeconds == 0 {
			new.TimeoutSeconds = existing.TimeoutSeconds
		}
		if new.CustomHeaders == nil {
			new.CustomHeaders = existing.CustomHeaders
		}
		merged, _ := json.Marshal(new)
		return merged
	}

	return newConfig
}

// isUniqueViolation checks if error is a unique constraint violation
// isIntegrationTeamFKViolation recognises the integrations -> teams foreign
// key, and only it. Matching on the constraint name rather than on the error
// class is deliberate: every other foreign key reachable from this table means
// something else, and laundering all of them into "team not found" would tell
// the caller to go fix a team that was never the problem.
func isIntegrationTeamFKViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) &&
		pqErr.Code.Name() == "foreign_key_violation" &&
		pqErr.Constraint == integrationTeamFKConstraint
}

func isUniqueViolation(err error) bool {
	// PostgreSQL error code for unique violation is 23505
	return err != nil && (err.Error() == "pq: duplicate key value violates unique constraint \"idx_integrations_type_outbound\"" ||
		err.Error() == "pq: duplicate key value violates unique constraint \"integrations_pkey\"")
}

// scopeToNullString converts *WebhookScope to sql.NullString for DB writes
func scopeToNullString(s *model.WebhookScope) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(*s), Valid: true}
}

// stringPtrToNullString converts *string to sql.NullString for DB writes
func stringPtrToNullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// SubscriberConfig reads one generic webhook integration's configuration for the
// delivery channel, under the caller's context.
//
// From the database, not the cache: the cache holds no generic webhooks and is
// refreshed only by the process that handled a change, so a secret rotated on
// one instance would leave every other signing with the old one. Under the
// caller's context, because the channel reads this inside an attempt whose
// every step has a deadline, and a read that could outlive them would hold a
// slot the deadlines exist to free.
//
// The bool is whether the subscriber exists; an id that names an integration of
// another type is not a subscriber either. An error is the database failing,
// and the two are different answers to a delivery.
func (s *Store) SubscriberConfig(ctx context.Context, integrationID string) (model.GenericWebhookConfig, bool, error) {
	var kind model.IntegrationType
	var encrypted string
	err := s.db.QueryRowContext(ctx,
		`SELECT type, config FROM integrations WHERE id = $1`, integrationID).Scan(&kind, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return model.GenericWebhookConfig{}, false, nil
	}
	if err != nil {
		return model.GenericWebhookConfig{}, false, err
	}
	if kind != model.IntegrationTypeGenericWebhook {
		return model.GenericWebhookConfig{}, false, nil
	}
	raw, err := decryptConfig(encrypted)
	if err != nil {
		return model.GenericWebhookConfig{}, false, err
	}
	var cfg model.GenericWebhookConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return model.GenericWebhookConfig{}, false, fmt.Errorf("subscriber %s: configuration does not read: %w", integrationID, err)
	}
	return cfg, true, nil
}
