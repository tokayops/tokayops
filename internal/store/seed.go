package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/model"
)

// Seed populates the database with initial data
func (s *Store) Seed() error {
	if err := s.seedEscalationPolicies(); err != nil {
		return err
	}
	if err := s.seedTeams(); err != nil {
		return err
	}
	if err := s.seedUsers(); err != nil {
		return err
	}
	if err := s.seedIntegrations(); err != nil {
		return err
	}
	if err := s.seedAlertGroups(); err != nil {
		return err
	}
	return nil
}

func (s *Store) seedEscalationPolicies() error {
	policies := []model.EscalationPolicy{
		{
			ID:          "default_policy",
			Name:        "Default Policy",
			Description: "Default escalation policy for all teams",
			Steps: []*model.EscalationStep{
				{
					ID:             "default_step_1",
					PolicyID:       "default_policy",
					StepIndex:      0,
					Provider:       "slack",
					TargetKind:     "channel",
					TargetType:     "channel",
					TargetID:       "C_ONCALL",
					DelaySeconds:   0,
					TimeoutSeconds: 30,
					MaxAttempts:    3,
				},
			},
		},
		{
			ID:          "critical_policy",
			Name:        "Critical Policy",
			Description: "Escalation policy for critical alerts",
			Steps: []*model.EscalationStep{
				{
					ID:             "critical_step_1",
					PolicyID:       "critical_policy",
					StepIndex:      0,
					Provider:       "slack",
					TargetKind:     "dm",
					TargetType:     "user",
					TargetID:       "admin",
					DelaySeconds:   0,
					TimeoutSeconds: 30,
					MaxAttempts:    5,
				},
			},
		},
	}

	for _, policy := range policies {
		_, err := s.GetEscalationPolicyByID(policy.ID)
		if err == sql.ErrNoRows {
			policy.CreatedAt = time.Now()
			policy.UpdatedAt = time.Now()
			if err := s.CreateEscalationPolicy(&policy); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) seedTeams() error {
	teams := []model.Team{
		{
			ID:              "devops",
			Name:            "DevOps Team",
			Description:     "Infrastructure and deployments",
			DefaultPolicyID: "default_policy",
			SeverityRoutes:  map[string]string{"critical": "critical_policy"},
		},
		{
			ID:              "billing",
			Name:            "Billing Team",
			Description:     "Payment and billing systems",
			DefaultPolicyID: "default_policy",
		},
		{
			ID:              "platform",
			Name:            "Platform Team",
			Description:     "Core platform services",
			DefaultPolicyID: "default_policy",
			SeverityRoutes:  map[string]string{"critical": "critical_policy"},
		},
		{
			ID:              "triage",
			Name:            "Triage",
			Description:     "Unassigned alerts triage",
			DefaultPolicyID: "default_policy",
		},
	}

	for _, team := range teams {
		_, errGet := s.GetTeamByID(team.ID)
		if errGet == sql.ErrNoRows {
			team.CreatedAt = time.Now()
			if err := s.CreateTeam(&team); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) seedUsers() error {
	// Password: Admin123!
	hash, _ := auth.HashPassword("Admin123!")

	users := []model.User{
		{ID: "admin", Email: "admin@example.com", Name: "Admin User", PasswordHash: hash},
		{ID: "alice", Email: "alice@example.com", Name: "Alice Smith", PasswordHash: hash},
		{ID: "bob", Email: "bob@example.com", Name: "Bob Johnson", PasswordHash: hash},
		{ID: "charlie", Email: "charlie@example.com", Name: "Charlie Brown", PasswordHash: hash},
	}

	for _, user := range users {
		_, errGet := s.GetUserByID(user.ID)
		if errGet == sql.ErrNoRows {
			user.CreatedAt = time.Now()
			if err := s.CreateUser(&user); err != nil {
				return err
			}
		}
	}

	// Add users to teams
	_ = s.AddTeamMember("devops", "admin", model.TeamMemberRoleAdmin)
	_ = s.AddTeamMember("devops", "alice", model.TeamMemberRoleMember)
	_ = s.AddTeamMember("billing", "bob", model.TeamMemberRoleAdmin)
	_ = s.AddTeamMember("platform", "charlie", model.TeamMemberRoleAdmin)
	_ = s.AddTeamMember("triage", "admin", model.TeamMemberRoleMember)

	return nil
}

func (s *Store) seedIntegrations() error {
	// Check if Slack integration exists
	_, err := s.GetIntegrationByType(model.IntegrationTypeSlack)
	if err == sql.ErrNoRows {
		// Create Slack integration (disabled - just for UI testing)
		slackCfg, _ := json.Marshal(model.SlackConfig{
			Token:          "xoxb-test-token-for-e2e",
			DefaultChannel: "C_ONCALL",
		})
		slackInt := &model.Integration{
			ID:        "integration-slack",
			Type:      model.IntegrationTypeSlack,
			Direction: model.IntegrationDirectionOutbound,
			Name:      "Slack (E2E Test)",
			Enabled:   false, // Disabled to not send real notifications
			Config:    slackCfg,
		}
		if err := s.CreateIntegration(slackInt); err != nil {
			return fmt.Errorf("create slack integration: %w", err)
		}
	}

	// Create Alertmanager webhook integration
	webhooks, _ := s.GetIntegrationsByType(model.IntegrationTypeAlertmanagerWebhook)
	if len(webhooks) == 0 {
		whCfg, _ := json.Marshal(model.WebhookConfig{
			Secret: "e2e-test-secret",
		})
		webhookInt := &model.Integration{
			ID:        "integration-alertmanager",
			Type:      model.IntegrationTypeAlertmanagerWebhook,
			Direction: model.IntegrationDirectionInbound,
			Name:      "Alertmanager Webhook (E2E Test)",
			Enabled:   true,
			Config:    whCfg,
		}
		if err := s.CreateIntegration(webhookInt); err != nil {
			return fmt.Errorf("create webhook integration: %w", err)
		}
	}

	return nil
}

func (s *Store) seedAlertGroups() error {
	alertGroups := []struct {
		Title    string
		TeamID   string
		Severity string
		Status   model.AlertGroupStatus
		Alerts   int
	}{
		// Triggered - Critical
		{"PostgresHighConnections", "devops", "critical", model.AlertGroupStatusTriggered, 3},
		{"KubernetesNodeNotReady", "devops", "critical", model.AlertGroupStatusTriggered, 2},
		{"PaymentGatewayTimeout", "billing", "critical", model.AlertGroupStatusTriggered, 5},

		// Triggered - Warning
		{"HighMemoryUsage", "devops", "warning", model.AlertGroupStatusTriggered, 2},
		{"DiskSpaceLow", "platform", "warning", model.AlertGroupStatusTriggered, 1},
		{"CacheHitRateLow", "platform", "warning", model.AlertGroupStatusTriggered, 3},

		// Triggered - Info
		{"PrometheusConfigReload", "devops", "info", model.AlertGroupStatusTriggered, 1},
		{"GrafanaDashboardUpdated", "platform", "info", model.AlertGroupStatusTriggered, 1},

		// Acknowledged - Critical
		{"VaultSealed", "devops", "critical", model.AlertGroupStatusAcknowledged, 2},
		{"DatabaseConnectionPoolExhausted", "billing", "critical", model.AlertGroupStatusAcknowledged, 3},

		// Acknowledged - Warning
		{"CPUThrottling", "platform", "warning", model.AlertGroupStatusAcknowledged, 2},

		// Resolved - Critical
		{"ConsulMemberDown", "devops", "critical", model.AlertGroupStatusResolved, 3},
		{"RedisMemoryHigh", "platform", "critical", model.AlertGroupStatusResolved, 1},

		// Resolved - Warning
		{"SSLCertificateExpiring", "devops", "warning", model.AlertGroupStatusResolved, 1},
		{"RabbitMQQueueHigh", "billing", "warning", model.AlertGroupStatusResolved, 2},

		// More for pagination testing (need 40+ alerts to ensure 2+ pages with pageSize=20)
		{"ElasticsearchClusterRed", "platform", "critical", model.AlertGroupStatusTriggered, 4},
		{"KafkaConsumerLag", "platform", "warning", model.AlertGroupStatusTriggered, 2},
		{"MongoDBReplicaSetUnhealthy", "devops", "critical", model.AlertGroupStatusTriggered, 3},
		{"NginxHighErrorRate", "platform", "warning", model.AlertGroupStatusTriggered, 2},
		{"APILatencyHigh", "billing", "warning", model.AlertGroupStatusTriggered, 4},
		{"DatabaseReplicationLag", "devops", "critical", model.AlertGroupStatusTriggered, 2},
		{"ServiceMeshLatencyHigh", "platform", "warning", model.AlertGroupStatusTriggered, 2},
		{"ContainerOOMKilled", "devops", "critical", model.AlertGroupStatusTriggered, 3},
		{"PodCrashLoopBackOff", "devops", "warning", model.AlertGroupStatusTriggered, 2},
		{"IngressControllerDown", "platform", "critical", model.AlertGroupStatusTriggered, 1},
		{"CertManagerRenewalFailed", "devops", "warning", model.AlertGroupStatusTriggered, 2},
		{"PrometheusTargetDown", "devops", "warning", model.AlertGroupStatusTriggered, 3},
		{"GrafanaHighLoad", "platform", "warning", model.AlertGroupStatusTriggered, 1},
		{"JaegerCollectorFailing", "platform", "warning", model.AlertGroupStatusTriggered, 2},
		{"FluentdBufferOverflow", "devops", "warning", model.AlertGroupStatusTriggered, 2},
		{"LokiIngesterFailing", "platform", "critical", model.AlertGroupStatusTriggered, 1},
		{"ThanosCompactorFailing", "devops", "warning", model.AlertGroupStatusTriggered, 2},
		{"AlertmanagerClusterFailing", "devops", "critical", model.AlertGroupStatusTriggered, 1},
		{"CoreDNSLatencyHigh", "platform", "warning", model.AlertGroupStatusTriggered, 2},
		{"ETCDHighLatency", "devops", "critical", model.AlertGroupStatusTriggered, 2},
		{"KubeAPIServerLatency", "devops", "warning", model.AlertGroupStatusTriggered, 3},
		{"SchedulerLatencyHigh", "platform", "warning", model.AlertGroupStatusTriggered, 1},
		{"ControllerManagerDown", "devops", "critical", model.AlertGroupStatusTriggered, 1},
		{"NodeDiskPressure", "platform", "warning", model.AlertGroupStatusTriggered, 4},
		{"NodeMemoryPressure", "devops", "warning", model.AlertGroupStatusTriggered, 3},
	}

	now := time.Now()
	for i, ag := range alertGroups {
		dedupKey := fmt.Sprintf("seed-%s-%d", ag.Title, i)

		// Check if already exists
		existing, _ := s.GetActiveAlertGroup(dedupKey)
		if existing != nil {
			continue
		}

		alerts := generateSeedAlerts(ag.Title, ag.Alerts, ag.Severity)

		// Resolve team name for snapshot — teams are seeded before alert groups
		teamName := ag.TeamID
		if team, err := s.GetTeamByID(ag.TeamID); err == nil {
			teamName = team.Name
		} else {
			log.Printf("Seed: WARNING: team %s not found for AG %s, using teamID as snapshot", ag.TeamID, ag.Title)
		}

		alertGroup := &model.AlertGroup{
			ID:               uuid.New().String(),
			DedupKey:         dedupKey,
			Status:           ag.Status,
			Title:            ag.Title,
			TeamID:           ag.TeamID,
			TeamNameSnapshot: teamName,
			Severity:         ag.Severity,
			PolicyID:         fmt.Sprintf("%s_default_policy", ag.TeamID),
			Alerts:           alerts,
			CreatedAt:        now.Add(-time.Duration(i*10) * time.Minute),
			UpdatedAt:        now,
		}

		if err := s.CreateAlertGroup(alertGroup); err != nil {
			continue // Skip if exists
		}

		// Add timeline events
		s.addSeedTimelineEvents(alertGroup.ID, ag.Title, now.Add(-time.Duration(i*10)*time.Minute))
	}

	return nil
}

func generateSeedAlerts(title string, count int, severity string) []model.Alert {
	alerts := make([]model.Alert, count)
	for i := 0; i < count; i++ {
		alerts[i] = model.Alert{
			Fingerprint: fmt.Sprintf("%s-%d", title, i),
			Status:      model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname":    title,
				"severity":     severity,
				"instance":     fmt.Sprintf("10.0.0.%d:9090", 100+i),
				"job":          "node-exporter",
				"dc":           fmt.Sprintf("dc%d", i%3+1),
				"env":          "production",
				"cluster":      fmt.Sprintf("k8s-cluster-%d", i%2+1),
				"namespace":    "monitoring",
				"pod":          fmt.Sprintf("pod-%s-%d", title, i),
				"container":    "exporter",
				"service":      fmt.Sprintf("svc-%s", title),
				"region":       "eu-west-1",
				"alertgroup":   "NodeExporter",
				"exported_job": fmt.Sprintf("integrations/%s", title),
			},
			Annotations: map[string]string{
				"summary":     fmt.Sprintf("%s alert on instance %d", title, i),
				"description": fmt.Sprintf("Detailed description for %s alert triggered on instance %d", title, i),
			},
			StartsAt: time.Now().Add(-time.Duration(i*5) * time.Minute),
		}
	}
	return alerts
}

func (s *Store) addSeedTimelineEvents(alertGroupID, title string, createdAt time.Time) {
	events := []*model.TimelineEvent{
		{
			ID:           uuid.New().String(),
			AlertGroupID: alertGroupID,
			Type:         model.TimelineEventCreated,
			Message:      fmt.Sprintf("Alert group created: %s", title),
			Actor:        "system",
			CreatedAt:    createdAt,
		},
		{
			ID:           uuid.New().String(),
			AlertGroupID: alertGroupID,
			Type:         model.TimelineEventAlertAdded,
			Message:      fmt.Sprintf("Alert: %s", title),
			Actor:        "system",
			CreatedAt:    createdAt.Add(1 * time.Second),
		},
		{
			ID:           uuid.New().String(),
			AlertGroupID: alertGroupID,
			Type:         model.TimelineEventNotificationSent,
			Message:      "Step 1 completed via slack_channel to #alerts-critical",
			Actor:        "dispatcher",
			Metadata: map[string]string{
				"step_type":    "slack_channel",
				"channel_id":   "C0123456789",
				"channel_name": "#alerts-critical",
			},
			CreatedAt: createdAt.Add(2 * time.Second),
		},
		{
			ID:           uuid.New().String(),
			AlertGroupID: alertGroupID,
			Type:         model.TimelineEventNotificationSent,
			Message:      "Step 2 completed via slack_dm to John Doe",
			Actor:        "dispatcher",
			Metadata: map[string]string{
				"step_type":     "slack_dm",
				"slack_user_id": "U0123456789",
				"user_name":     "John Doe",
			},
			CreatedAt: createdAt.Add(3 * time.Second),
		},
	}

	for _, e := range events {
		_ = s.AddTimelineEvent(e)
	}
}
