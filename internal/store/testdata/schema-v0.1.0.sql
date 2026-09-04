-- The schema of TokayOps v0.1.0, exactly as that release's own start built
-- it: dumped with pg_dump --schema-only --no-owner --no-privileges from an
-- empty database that v0.1.0's InitDB had just run against, with the psql
-- meta-commands and session settings removed. Nothing here is written by
-- hand. It is what every installation on v0.1.0 has when this version starts,
-- and the upgrade test starts from it.
--
-- To regenerate: check the tag out (git worktree add <dir> v0.1.0), run its
-- InitDB against an empty database, dump that database the same way, and
-- strip the \restrict, SET and set_config lines.

-- Name: btree_gist; Type: EXTENSION; Schema: -; Owner: -

CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;

-- Name: EXTENSION btree_gist; Type: COMMENT; Schema: -; Owner: -

COMMENT ON EXTENSION btree_gist IS 'support for indexing common datatypes in GiST';

-- Name: alert_groups; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.alert_groups (
    id text NOT NULL,
    dedup_key text NOT NULL,
    status text NOT NULL,
    title text,
    team_id text NOT NULL,
    severity text,
    policy_id text,
    current_step integer DEFAULT 0,
    notification_states text,
    external_url text,
    alerts_data text,
    acknowledged_by text,
    resolved_by text,
    incident_id integer,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    resolved_at timestamp with time zone,
    policy_snapshot jsonb,
    oncall_snapshot jsonb,
    ack_processed_at timestamp with time zone,
    slack_update_pending boolean DEFAULT false NOT NULL,
    team_name_snapshot text NOT NULL
);

-- Name: api_tokens; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.api_tokens (
    id text NOT NULL,
    user_id text NOT NULL,
    name text NOT NULL,
    token_hash text NOT NULL,
    expires_at timestamp with time zone,
    last_used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP
);

-- Name: escalation_policies; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.escalation_policies (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    team_id text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP
);

-- Name: escalation_steps; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.escalation_steps (
    id text NOT NULL,
    policy_id text NOT NULL,
    step_index integer NOT NULL,
    provider text NOT NULL,
    target_kind text NOT NULL,
    target_type text NOT NULL,
    target_id text NOT NULL,
    delay_seconds integer DEFAULT 0,
    timeout_seconds integer DEFAULT 30,
    max_attempts integer DEFAULT 5,
    message text,
    continue_on_failure boolean DEFAULT true,
    CONSTRAINT escalation_steps_delay_seconds_check CHECK ((delay_seconds >= 0)),
    CONSTRAINT escalation_steps_max_attempts_check CHECK ((max_attempts >= 1)),
    CONSTRAINT escalation_steps_step_index_check CHECK ((step_index >= 0)),
    CONSTRAINT escalation_steps_target_kind_check CHECK ((target_kind = ANY (ARRAY['dm'::text, 'channel'::text]))),
    CONSTRAINT escalation_steps_target_type_check CHECK ((target_type = ANY (ARRAY['user'::text, 'channel'::text, 'schedule'::text]))),
    CONSTRAINT escalation_steps_timeout_seconds_check CHECK ((timeout_seconds > 0))
);

-- Name: event_outbox; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.event_outbox (
    id text NOT NULL,
    event_type text NOT NULL,
    alert_group_id text NOT NULL,
    team_id text NOT NULL,
    actor text DEFAULT 'system'::text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone,
    locked_until timestamp with time zone,
    locked_by text,
    last_error text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    sent_at timestamp with time zone
);

-- Name: event_outbox_deliveries; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.event_outbox_deliveries (
    id text NOT NULL,
    event_id text NOT NULL,
    integration_id text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone,
    last_http_status integer,
    last_error text,
    request_payload text,
    response_body_trunc text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    sent_at timestamp with time zone
);

-- Name: event_outbox_delivery_attempts; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.event_outbox_delivery_attempts (
    id text NOT NULL,
    delivery_id text NOT NULL,
    attempt integer NOT NULL,
    http_status integer,
    error text,
    response_body_trunc text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

-- Name: external_identities; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.external_identities (
    id text NOT NULL,
    user_id text NOT NULL,
    provider text NOT NULL,
    external_id text NOT NULL,
    chat_id text,
    display_name text,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now()
);

-- Name: incidents; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.incidents (
    id integer NOT NULL,
    title text NOT NULL,
    status text DEFAULT 'investigating'::text,
    severity text,
    commander_id text,
    slack_channel_id text,
    created_at timestamp with time zone DEFAULT now()
);

-- Name: incidents_id_seq; Type: SEQUENCE; Schema: public; Owner: -

CREATE SEQUENCE public.incidents_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

-- Name: incidents_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -

ALTER SEQUENCE public.incidents_id_seq OWNED BY public.incidents.id;

-- Name: integrations; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.integrations (
    id text NOT NULL,
    type text NOT NULL,
    direction text NOT NULL,
    name text NOT NULL,
    enabled boolean DEFAULT true,
    config text NOT NULL,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now(),
    scope text,
    team_id text,
    CONSTRAINT chk_webhook_scope CHECK ((((type = 'generic_webhook'::text) AND (scope = ANY (ARRAY['global'::text, 'team'::text]))) OR ((type <> 'generic_webhook'::text) AND (scope IS NULL)))),
    CONSTRAINT chk_webhook_team_id CHECK ((((scope = 'team'::text) AND (team_id IS NOT NULL)) OR ((scope IS DISTINCT FROM 'team'::text) AND (team_id IS NULL))))
);

-- Name: job_stages; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.job_stages (
    id text NOT NULL,
    job_id text NOT NULL,
    stage_index integer NOT NULL,
    status text DEFAULT 'blocked'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now()
);

-- Name: job_steps; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.job_steps (
    id text NOT NULL,
    job_id text NOT NULL,
    stage_id text NOT NULL,
    step_index integer NOT NULL,
    step_type text NOT NULL,
    status text NOT NULL,
    data text DEFAULT '{}'::text NOT NULL,
    result text,
    error text,
    next_run_at timestamp with time zone,
    locked_until timestamp with time zone,
    locked_by text,
    attempt_count integer DEFAULT 0,
    timeout_seconds integer,
    max_attempts integer DEFAULT 5,
    continue_on_failure boolean DEFAULT false,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now()
);

-- Name: jobs; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.jobs (
    id text NOT NULL,
    type text NOT NULL,
    status text NOT NULL,
    payload text DEFAULT '{}'::text NOT NULL,
    dedup_key text,
    alert_group_id text,
    current_stage integer DEFAULT 0,
    error text,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now(),
    finished_at timestamp with time zone,
    canceled_at timestamp with time zone
);

-- Name: link_tokens; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.link_tokens (
    id text NOT NULL,
    user_id text NOT NULL,
    provider text NOT NULL,
    token_hash text NOT NULL,
    external_id text,
    attempts integer DEFAULT 0,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now()
);

-- Name: migration_markers; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.migration_markers (
    name text NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);

-- Name: notification_deliveries; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.notification_deliveries (
    id text NOT NULL,
    alert_group_id text NOT NULL,
    job_step_id text,
    provider text NOT NULL,
    kind text NOT NULL,
    target_type text,
    target_id text,
    provider_payload text,
    supports_update boolean DEFAULT false,
    is_primary boolean DEFAULT false,
    is_firehose boolean DEFAULT false,
    attempt integer DEFAULT 0,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now()
);

-- Name: schedule_override_revisions; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.schedule_override_revisions (
    revision_id text NOT NULL,
    override_id text NOT NULL,
    schedule_id text NOT NULL,
    revision bigint NOT NULL,
    layer text DEFAULT 'l1'::text NOT NULL,
    user_id text NOT NULL,
    valid_from timestamp with time zone NOT NULL,
    valid_to timestamp with time zone NOT NULL,
    reason text,
    deleted boolean DEFAULT false NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    recorded_by text,
    CONSTRAINT schedule_override_revisions_check CHECK ((valid_to > valid_from)),
    CONSTRAINT schedule_override_revisions_layer_check CHECK ((layer = ANY (ARRAY['l1'::text, 'l2'::text]))),
    CONSTRAINT schedule_override_revisions_revision_positive CHECK ((revision >= 1))
);

-- Name: schedule_revisions; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.schedule_revisions (
    id text NOT NULL,
    schedule_id text NOT NULL,
    version bigint NOT NULL,
    kind text DEFAULT 'active'::text NOT NULL,
    snapshot jsonb NOT NULL,
    effective_from timestamp with time zone NOT NULL,
    effective_to timestamp with time zone,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by text,
    change_reason text,
    change_summary jsonb,
    CONSTRAINT schedule_revisions_check CHECK (((effective_to IS NULL) OR (effective_to > effective_from))),
    CONSTRAINT schedule_revisions_kind_known CHECK ((kind = ANY (ARRAY['active'::text, 'deleted'::text]))),
    CONSTRAINT schedule_revisions_version_positive CHECK ((version >= 1))
);

-- Name: schedules; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.schedules (
    id text NOT NULL,
    team_id text NOT NULL,
    config_version bigint DEFAULT 0 NOT NULL,
    history_complete_from timestamp with time zone NOT NULL,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP
);

-- Name: team_members; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.team_members (
    team_id text NOT NULL,
    user_id text NOT NULL,
    role text DEFAULT 'team_member'::text
);

-- Name: teams; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.teams (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    slack_channel text,
    created_at timestamp with time zone,
    default_policy_id text,
    severity_routes jsonb DEFAULT '{}'::jsonb
);

-- Name: timeline_events; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.timeline_events (
    id text NOT NULL,
    alert_group_id text NOT NULL,
    type text NOT NULL,
    message text NOT NULL,
    actor text DEFAULT 'system'::text,
    metadata text DEFAULT '{}'::text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP
);

-- Name: users; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.users (
    id text NOT NULL,
    email text,
    name text NOT NULL,
    password_hash text,
    auth_provider text,
    created_at timestamp with time zone,
    deleted_at timestamp with time zone,
    role text DEFAULT 'user'::text
);

-- Name: incidents id; Type: DEFAULT; Schema: public; Owner: -

ALTER TABLE ONLY public.incidents ALTER COLUMN id SET DEFAULT nextval('public.incidents_id_seq'::regclass);

-- Name: alert_groups alert_groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.alert_groups
    ADD CONSTRAINT alert_groups_pkey PRIMARY KEY (id);

-- Name: api_tokens api_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_pkey PRIMARY KEY (id);

-- Name: api_tokens api_tokens_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_token_hash_key UNIQUE (token_hash);

-- Name: escalation_policies escalation_policies_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.escalation_policies
    ADD CONSTRAINT escalation_policies_pkey PRIMARY KEY (id);

-- Name: escalation_steps escalation_steps_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.escalation_steps
    ADD CONSTRAINT escalation_steps_pkey PRIMARY KEY (id);

-- Name: escalation_steps escalation_steps_policy_id_step_index_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.escalation_steps
    ADD CONSTRAINT escalation_steps_policy_id_step_index_key UNIQUE (policy_id, step_index);

-- Name: event_outbox_deliveries event_outbox_deliveries_event_id_integration_id_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.event_outbox_deliveries
    ADD CONSTRAINT event_outbox_deliveries_event_id_integration_id_key UNIQUE (event_id, integration_id);

-- Name: event_outbox_deliveries event_outbox_deliveries_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.event_outbox_deliveries
    ADD CONSTRAINT event_outbox_deliveries_pkey PRIMARY KEY (id);

-- Name: event_outbox_delivery_attempts event_outbox_delivery_attempts_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.event_outbox_delivery_attempts
    ADD CONSTRAINT event_outbox_delivery_attempts_pkey PRIMARY KEY (id);

-- Name: event_outbox event_outbox_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.event_outbox
    ADD CONSTRAINT event_outbox_pkey PRIMARY KEY (id);

-- Name: external_identities external_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.external_identities
    ADD CONSTRAINT external_identities_pkey PRIMARY KEY (id);

-- Name: external_identities external_identities_user_id_provider_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.external_identities
    ADD CONSTRAINT external_identities_user_id_provider_key UNIQUE (user_id, provider);

-- Name: incidents incidents_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.incidents
    ADD CONSTRAINT incidents_pkey PRIMARY KEY (id);

-- Name: integrations integrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.integrations
    ADD CONSTRAINT integrations_pkey PRIMARY KEY (id);

-- Name: job_stages job_stages_job_id_stage_index_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.job_stages
    ADD CONSTRAINT job_stages_job_id_stage_index_key UNIQUE (job_id, stage_index);

-- Name: job_stages job_stages_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.job_stages
    ADD CONSTRAINT job_stages_pkey PRIMARY KEY (id);

-- Name: job_steps job_steps_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.job_steps
    ADD CONSTRAINT job_steps_pkey PRIMARY KEY (id);

-- Name: jobs jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_pkey PRIMARY KEY (id);

-- Name: link_tokens link_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.link_tokens
    ADD CONSTRAINT link_tokens_pkey PRIMARY KEY (id);

-- Name: link_tokens link_tokens_provider_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.link_tokens
    ADD CONSTRAINT link_tokens_provider_token_hash_key UNIQUE (provider, token_hash);

-- Name: link_tokens link_tokens_user_id_provider_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.link_tokens
    ADD CONSTRAINT link_tokens_user_id_provider_key UNIQUE (user_id, provider);

-- Name: migration_markers migration_markers_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.migration_markers
    ADD CONSTRAINT migration_markers_pkey PRIMARY KEY (name);

-- Name: schedule_revisions no_overlapping_schedule_revisions; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedule_revisions
    ADD CONSTRAINT no_overlapping_schedule_revisions EXCLUDE USING gist (schedule_id WITH =, tstzrange(effective_from, effective_to, '[)'::text) WITH &&);

-- Name: notification_deliveries notification_deliveries_job_step_id_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.notification_deliveries
    ADD CONSTRAINT notification_deliveries_job_step_id_key UNIQUE (job_step_id);

-- Name: notification_deliveries notification_deliveries_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.notification_deliveries
    ADD CONSTRAINT notification_deliveries_pkey PRIMARY KEY (id);

-- Name: schedule_override_revisions schedule_override_revisions_override_id_revision_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedule_override_revisions
    ADD CONSTRAINT schedule_override_revisions_override_id_revision_key UNIQUE (override_id, revision);

-- Name: schedule_override_revisions schedule_override_revisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedule_override_revisions
    ADD CONSTRAINT schedule_override_revisions_pkey PRIMARY KEY (revision_id);

-- Name: schedule_revisions schedule_revisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedule_revisions
    ADD CONSTRAINT schedule_revisions_pkey PRIMARY KEY (id);

-- Name: schedule_revisions schedule_revisions_schedule_id_version_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedule_revisions
    ADD CONSTRAINT schedule_revisions_schedule_id_version_key UNIQUE (schedule_id, version);

-- Name: schedules schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedules
    ADD CONSTRAINT schedules_pkey PRIMARY KEY (id);

-- Name: schedules schedules_team_id_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedules
    ADD CONSTRAINT schedules_team_id_key UNIQUE (team_id);

-- Name: team_members team_members_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.team_members
    ADD CONSTRAINT team_members_pkey PRIMARY KEY (team_id, user_id);

-- Name: teams teams_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_pkey PRIMARY KEY (id);

-- Name: timeline_events timeline_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.timeline_events
    ADD CONSTRAINT timeline_events_pkey PRIMARY KEY (id);

-- Name: users users_email_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_email_key UNIQUE (email);

-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);

-- Name: idx_active_alert_groups; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_active_alert_groups ON public.alert_groups USING btree (dedup_key) WHERE (status <> ALL (ARRAY['resolved'::text, 'closed'::text]));

-- Name: idx_active_jobs_dedup; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_active_jobs_dedup ON public.jobs USING btree (dedup_key) WHERE (status = ANY (ARRAY['pending'::text, 'running'::text]));

-- Name: idx_alert_groups_status; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_alert_groups_status ON public.alert_groups USING btree (status);

-- Name: idx_alert_groups_team_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_alert_groups_team_id ON public.alert_groups USING btree (team_id);

-- Name: idx_api_tokens_hash; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_api_tokens_hash ON public.api_tokens USING btree (token_hash);

-- Name: idx_api_tokens_user; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_api_tokens_user ON public.api_tokens USING btree (user_id);

-- Name: idx_delivery_attempts_delivery; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_delivery_attempts_delivery ON public.event_outbox_delivery_attempts USING btree (delivery_id);

-- Name: idx_delivery_event; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_delivery_event ON public.event_outbox_deliveries USING btree (event_id);

-- Name: idx_delivery_retry; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_delivery_retry ON public.event_outbox_deliveries USING btree (next_attempt_at) WHERE (status = ANY (ARRAY['pending'::text, 'retry'::text]));

-- Name: idx_escalation_steps_policy_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_escalation_steps_policy_id ON public.escalation_steps USING btree (policy_id);

-- Name: idx_external_identities_provider_external; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_external_identities_provider_external ON public.external_identities USING btree (provider, external_id);

-- Name: idx_integrations_type_outbound; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_integrations_type_outbound ON public.integrations USING btree (type) WHERE ((direction = 'outbound'::text) AND (type <> 'generic_webhook'::text));

-- Name: idx_job_stages_job_status; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_job_stages_job_status ON public.job_stages USING btree (job_id, status);

-- Name: idx_job_steps_job_id_index; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_job_steps_job_id_index ON public.job_steps USING btree (job_id, step_index);

-- Name: idx_job_steps_status_next_run; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_job_steps_status_next_run ON public.job_steps USING btree (status, next_run_at) WHERE (status = ANY (ARRAY['pending'::text, 'retry'::text]));

-- Name: idx_jobs_status; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_jobs_status ON public.jobs USING btree (status);

-- Name: idx_notification_deliveries_alert_group; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_notification_deliveries_alert_group ON public.notification_deliveries USING btree (alert_group_id);

-- Name: idx_notification_deliveries_firehose; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_notification_deliveries_firehose ON public.notification_deliveries USING btree (alert_group_id, is_firehose);

-- Name: idx_notification_deliveries_primary; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_notification_deliveries_primary ON public.notification_deliveries USING btree (alert_group_id, is_primary);

-- Name: idx_one_escalation_per_ag; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_one_escalation_per_ag ON public.jobs USING btree (alert_group_id) WHERE ((type = 'escalation'::text) AND (alert_group_id IS NOT NULL));

-- Name: idx_outbox_alert_group; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbox_alert_group ON public.event_outbox USING btree (alert_group_id);

-- Name: idx_outbox_claim; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbox_claim ON public.event_outbox USING btree (next_attempt_at) WHERE (status = ANY (ARRAY['pending'::text, 'processing'::text]));

-- Name: idx_schedule_override_revisions_range; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_schedule_override_revisions_range ON public.schedule_override_revisions USING btree (schedule_id, valid_from);

-- Name: idx_schedule_revisions_one_tail; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_schedule_revisions_one_tail ON public.schedule_revisions USING btree (schedule_id) WHERE (effective_to IS NULL);

-- Name: idx_schedule_revisions_range; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_schedule_revisions_range ON public.schedule_revisions USING btree (schedule_id, effective_from);

-- Name: idx_timeline_alert_group; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_timeline_alert_group ON public.timeline_events USING btree (alert_group_id);

-- Name: api_tokens api_tokens_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.api_tokens
    ADD CONSTRAINT api_tokens_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

-- Name: escalation_policies escalation_policies_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.escalation_policies
    ADD CONSTRAINT escalation_policies_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE SET NULL;

-- Name: escalation_steps escalation_steps_policy_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.escalation_steps
    ADD CONSTRAINT escalation_steps_policy_id_fkey FOREIGN KEY (policy_id) REFERENCES public.escalation_policies(id) ON DELETE CASCADE;

-- Name: event_outbox event_outbox_alert_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.event_outbox
    ADD CONSTRAINT event_outbox_alert_group_id_fkey FOREIGN KEY (alert_group_id) REFERENCES public.alert_groups(id);

-- Name: event_outbox_deliveries event_outbox_deliveries_event_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.event_outbox_deliveries
    ADD CONSTRAINT event_outbox_deliveries_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.event_outbox(id);

-- Name: event_outbox_deliveries event_outbox_deliveries_integration_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.event_outbox_deliveries
    ADD CONSTRAINT event_outbox_deliveries_integration_id_fkey FOREIGN KEY (integration_id) REFERENCES public.integrations(id);

-- Name: event_outbox_delivery_attempts event_outbox_delivery_attempts_delivery_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.event_outbox_delivery_attempts
    ADD CONSTRAINT event_outbox_delivery_attempts_delivery_id_fkey FOREIGN KEY (delivery_id) REFERENCES public.event_outbox_deliveries(id);

-- Name: external_identities external_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.external_identities
    ADD CONSTRAINT external_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

-- Name: integrations integrations_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.integrations
    ADD CONSTRAINT integrations_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id);

-- Name: job_stages job_stages_job_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.job_stages
    ADD CONSTRAINT job_stages_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.jobs(id) ON DELETE CASCADE;

-- Name: job_steps job_steps_job_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.job_steps
    ADD CONSTRAINT job_steps_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.jobs(id) ON DELETE CASCADE;

-- Name: job_steps job_steps_stage_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.job_steps
    ADD CONSTRAINT job_steps_stage_id_fkey FOREIGN KEY (stage_id) REFERENCES public.job_stages(id);

-- Name: link_tokens link_tokens_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.link_tokens
    ADD CONSTRAINT link_tokens_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

-- Name: notification_deliveries notification_deliveries_alert_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.notification_deliveries
    ADD CONSTRAINT notification_deliveries_alert_group_id_fkey FOREIGN KEY (alert_group_id) REFERENCES public.alert_groups(id);

-- Name: notification_deliveries notification_deliveries_job_step_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.notification_deliveries
    ADD CONSTRAINT notification_deliveries_job_step_id_fkey FOREIGN KEY (job_step_id) REFERENCES public.job_steps(id) ON DELETE SET NULL;

-- Name: schedule_override_revisions schedule_override_revisions_schedule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedule_override_revisions
    ADD CONSTRAINT schedule_override_revisions_schedule_id_fkey FOREIGN KEY (schedule_id) REFERENCES public.schedules(id) ON DELETE RESTRICT;

-- Name: schedule_revisions schedule_revisions_schedule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedule_revisions
    ADD CONSTRAINT schedule_revisions_schedule_id_fkey FOREIGN KEY (schedule_id) REFERENCES public.schedules(id) ON DELETE RESTRICT;

-- Name: schedules schedules_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.schedules
    ADD CONSTRAINT schedules_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;

-- Name: team_members team_members_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.team_members
    ADD CONSTRAINT team_members_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id);

-- Name: team_members team_members_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.team_members
    ADD CONSTRAINT team_members_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id);

-- Name: timeline_events timeline_events_alert_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.timeline_events
    ADD CONSTRAINT timeline_events_alert_group_id_fkey FOREIGN KEY (alert_group_id) REFERENCES public.alert_groups(id);

