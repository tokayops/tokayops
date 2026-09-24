-- The schema of TokayOps v0.2.0, exactly as that release's own start built
-- it: dumped with pg_dump --schema-only --no-owner --no-privileges from an
-- empty database that v0.2.0's InitDB had just run against, with the psql
-- meta-commands and session settings removed. Nothing here is written by
-- hand. It is what every installation on v0.2.0 has when this version starts,
-- and the upgrade test starts from it.
--
-- To regenerate: check the tag out (git worktree add <dir> v0.2.0), run its
-- InitDB against an empty database, dump that database the same way, and
-- strip the \restrict, SET and set_config lines.

-- Name: btree_gist; Type: EXTENSION; Schema: -; Owner: -

CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;

-- Name: EXTENSION btree_gist; Type: COMMENT; Schema: -; Owner: -

COMMENT ON EXTENSION btree_gist IS 'support for indexing common datatypes in GiST';

-- Name: alert_groups; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.alert_groups (
    id text NOT NULL,
    alert_key text NOT NULL,
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
    render_source_version bigint DEFAULT 0 NOT NULL,
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
    sent_at timestamp with time zone,
    fanned_out_at timestamp with time zone
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

-- Name: integration_tombstones; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.integration_tombstones (
    id text NOT NULL,
    type text NOT NULL,
    scope text,
    team_id text,
    deleted_at timestamp with time zone DEFAULT now() NOT NULL
);

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

-- Name: outbound_attempt_observations; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.outbound_attempt_observations (
    id text NOT NULL,
    attempt_id text NOT NULL,
    observation_kind text NOT NULL,
    outcome text NOT NULL,
    error_class text,
    provider_status text,
    receipt jsonb,
    receipt_recorded boolean DEFAULT false NOT NULL,
    receipt_redacted_at timestamp with time zone,
    applied_revision bigint,
    provider_result_detail text,
    response_summary text,
    completion_fingerprint bytea NOT NULL,
    completion_fingerprint_version integer NOT NULL,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT outbound_attempt_observations_digest_len CHECK ((octet_length(completion_fingerprint) = 32)),
    CONSTRAINT outbound_receipt_state CHECK ((((NOT receipt_recorded) AND (receipt IS NULL) AND (receipt_redacted_at IS NULL)) OR (receipt_recorded AND (receipt IS NOT NULL) AND (receipt_redacted_at IS NULL)) OR (receipt_recorded AND (receipt IS NULL) AND (receipt_redacted_at IS NOT NULL))))
);

-- Name: outbound_attempts; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.outbound_attempts (
    id text NOT NULL,
    intent_id text NOT NULL,
    attempt_no integer NOT NULL,
    record_kind text NOT NULL,
    generation_no integer NOT NULL,
    attempt_kind text NOT NULL,
    operation text NOT NULL,
    applied_revision bigint,
    provider text NOT NULL,
    bound_endpoint text,
    provider_key text,
    request_fingerprint bytea,
    lease_token text,
    worker_id text,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    outcome text,
    error_class text,
    provider_status text,
    receipt jsonb,
    receipt_recorded boolean DEFAULT false NOT NULL,
    receipt_redacted_at timestamp with time zone,
    response_summary text,
    finish_reason text,
    provider_result_detail text,
    completion_fingerprint bytea,
    completion_fingerprint_version integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT outbound_attempts_digest_len CHECK (((completion_fingerprint IS NULL) OR (octet_length(completion_fingerprint) = 32))),
    CONSTRAINT outbound_attempts_fingerprint_shape CHECK (((record_kind <> 'attempt'::text) OR ((completion_fingerprint_version IS NOT NULL) AND ((finished_at IS NULL) = (completion_fingerprint IS NULL))))),
    CONSTRAINT outbound_attempts_finished_shape CHECK ((((finished_at IS NULL) AND (outcome IS NULL) AND (finish_reason IS NULL)) OR ((finished_at IS NOT NULL) AND (outcome IS NOT NULL) AND (finish_reason IS NOT NULL) AND ((started_at IS NULL) OR (finished_at >= started_at))))),
    CONSTRAINT outbound_attempts_kind_shape CHECK ((((record_kind = 'attempt'::text) AND (started_at IS NOT NULL) AND (lease_token IS NOT NULL)) OR ((record_kind = 'preparation'::text) AND (started_at IS NULL) AND (lease_token IS NULL) AND (finished_at IS NOT NULL) AND (finish_reason = 'preparation'::text) AND (completion_fingerprint IS NULL) AND (completion_fingerprint_version IS NULL)))),
    CONSTRAINT outbound_attempts_result_detail_known CHECK (((provider_result_detail IS NULL) OR (provider_result_detail = ANY (ARRAY['acceptance_proven'::text, 'delivery_proven'::text, 'definitely_absent'::text, 'inconclusive'::text])))),
    CONSTRAINT outbound_receipt_state CHECK ((((NOT receipt_recorded) AND (receipt IS NULL) AND (receipt_redacted_at IS NULL)) OR (receipt_recorded AND (receipt IS NOT NULL) AND (receipt_redacted_at IS NULL)) OR (receipt_recorded AND (receipt IS NULL) AND (receipt_redacted_at IS NOT NULL))))
);

-- Name: outbound_batches; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.outbound_batches (
    id text NOT NULL,
    batch_key text NOT NULL,
    key_kind text NOT NULL,
    delivery_family text NOT NULL,
    grammar_version integer NOT NULL,
    alert_group_id text,
    event_id text,
    fingerprint bytea NOT NULL,
    fingerprint_version integer NOT NULL,
    admission_outcome text NOT NULL,
    intent_count integer NOT NULL,
    admission_snapshot jsonb,
    admission_digest bytea,
    admission_schema_version integer,
    admission_revision bigint,
    admitted_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT outbound_batches_admission_snapshot_present CHECK (((key_kind <> ALL (ARRAY['escalation'::text, 'escalation_replay'::text])) OR ((admission_snapshot IS NOT NULL) AND (admission_digest IS NOT NULL) AND (admission_schema_version IS NOT NULL) AND (admission_revision IS NOT NULL)))),
    CONSTRAINT outbound_batches_admission_snapshot_shape CHECK ((((admission_snapshot IS NULL) AND (admission_digest IS NULL) AND (admission_schema_version IS NULL) AND (admission_revision IS NULL)) OR ((admission_snapshot IS NOT NULL) AND (admission_digest IS NOT NULL) AND (admission_schema_version IS NOT NULL) AND (admission_revision IS NOT NULL) AND (octet_length(admission_digest) = 32) AND (admission_revision >= 0)))),
    CONSTRAINT outbound_batches_count_nonneg CHECK ((intent_count >= 0)),
    CONSTRAINT outbound_batches_family_matches_kind_v2 CHECK ((((key_kind = ANY (ARRAY['escalation'::text, 'escalation_replay'::text])) AND (delivery_family = 'notification'::text)) OR ((key_kind = 'handoff'::text) AND (delivery_family = 'handoff'::text)) OR ((key_kind = ANY (ARRAY['webhook_event'::text, 'webhook_replay'::text])) AND (delivery_family = 'webhook'::text)))),
    CONSTRAINT outbound_batches_fingerprint_len CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT outbound_batches_outcome_known CHECK ((admission_outcome = ANY (ARRAY['admitted'::text, 'no_targets'::text]))),
    CONSTRAINT outbound_batches_outcome_shape CHECK (((admission_outcome = 'no_targets'::text) = (intent_count = 0))),
    CONSTRAINT outbound_batches_webhook_names_event CHECK (((key_kind = ANY (ARRAY['webhook_event'::text, 'webhook_replay'::text])) = (event_id IS NOT NULL)))
);

-- Name: outbound_group_snapshots; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.outbound_group_snapshots (
    alert_group_id text NOT NULL,
    revision bigint NOT NULL,
    snapshot_schema_version integer NOT NULL,
    snapshot jsonb NOT NULL,
    snapshot_digest bytea NOT NULL,
    final boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    card_digest bytea NOT NULL,
    thread_digest bytea NOT NULL,
    CONSTRAINT outbound_group_snapshots_digest_len CHECK ((octet_length(snapshot_digest) = 32)),
    CONSTRAINT outbound_group_snapshots_form_digest_len CHECK (((octet_length(card_digest) = 32) AND (octet_length(thread_digest) = 32))),
    CONSTRAINT outbound_group_snapshots_revision CHECK ((revision >= 0))
);

-- Name: outbound_intent_events; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.outbound_intent_events (
    id text NOT NULL,
    intent_id text NOT NULL,
    seq integer NOT NULL,
    kind text NOT NULL,
    reason text,
    actor text,
    actor_kind text NOT NULL,
    from_status text,
    to_status text,
    generation_no integer,
    detail jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT outbound_intent_events_actor_kind CHECK ((actor_kind = ANY (ARRAY['user'::text, 'system'::text, 'legacy'::text])))
);

-- Name: outbound_intents; Type: TABLE; Schema: public; Owner: -

CREATE TABLE public.outbound_intents (
    id text NOT NULL,
    batch_id text NOT NULL,
    idempotency_key text NOT NULL,
    delivery_family text NOT NULL,
    key_kind text NOT NULL,
    grammar_version integer NOT NULL,
    provider text NOT NULL,
    target_kind text NOT NULL,
    target_ref text NOT NULL,
    alert_group_id text,
    form text NOT NULL,
    completion_mode text NOT NULL,
    ambiguity_policy text NOT NULL,
    payload_schema_version integer NOT NULL,
    payload jsonb NOT NULL,
    provider_key_codec_version integer NOT NULL,
    status text NOT NULL,
    generation_no integer DEFAULT 0 NOT NULL,
    attempts_in_generation integer DEFAULT 0 NOT NULL,
    failure_streak integer DEFAULT 0 NOT NULL,
    bound_endpoint text,
    create_key text,
    receipt jsonb,
    receipt_ref text,
    payload_digest bytea NOT NULL,
    receipt_recorded boolean DEFAULT false NOT NULL,
    receipt_redacted_at timestamp with time zone,
    recipient_erased_at timestamp with time zone,
    desired_revision bigint DEFAULT 0 NOT NULL,
    applied_revision bigint,
    final_revision_applied boolean DEFAULT false NOT NULL,
    cancellation_requested boolean DEFAULT false NOT NULL,
    accepted_duplicate_risk boolean DEFAULT false NOT NULL,
    not_before timestamp with time zone NOT NULL,
    next_attempt_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone,
    receipt_timeout_at timestamp with time zone,
    current_attempt_id text,
    lease_token text,
    locked_until timestamp with time zone,
    worker_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    bound_context jsonb,
    parent_intent_id text,
    awaits_intent_ids text[],
    CONSTRAINT outbound_intents_cancel_flag_on_sending CHECK (((NOT cancellation_requested) OR (status = 'sending'::text))),
    CONSTRAINT outbound_intents_counters_nonneg CHECK (((attempts_in_generation >= 0) AND (failure_streak >= 0) AND (generation_no >= 0))),
    CONSTRAINT outbound_intents_family_matches_kind_v2 CHECK ((((key_kind = ANY (ARRAY['escalation'::text, 'escalation_replay'::text])) AND (delivery_family = 'notification'::text)) OR ((key_kind = 'handoff'::text) AND (delivery_family = 'handoff'::text)) OR ((key_kind = ANY (ARRAY['webhook_event'::text, 'webhook_replay'::text])) AND (delivery_family = 'webhook'::text)))),
    CONSTRAINT outbound_intents_form_known CHECK ((form = ANY (ARRAY['editable'::text, 'one_shot'::text]))),
    CONSTRAINT outbound_intents_handoff_payload_addresses_the_target CHECK (((key_kind <> 'handoff'::text) OR ((NOT (((payload -> 'target'::text) ->> 'kind'::text) IS DISTINCT FROM target_kind)) AND (NOT (((payload -> 'target'::text) ->> 'ref'::text) IS DISTINCT FROM target_ref))))),
    CONSTRAINT outbound_intents_handoff_targets_a_person CHECK (((key_kind <> 'handoff'::text) OR (target_kind = 'user'::text))),
    CONSTRAINT outbound_intents_lease_only_when_working CHECK (((lease_token IS NULL) OR (status = ANY (ARRAY['pending'::text, 'sending'::text])))),
    CONSTRAINT outbound_intents_lease_pair CHECK (((lease_token IS NULL) = (locked_until IS NULL))),
    CONSTRAINT outbound_intents_payload_digest_len CHECK ((octet_length(payload_digest) = 32)),
    CONSTRAINT outbound_intents_receipt_is_named CHECK ((((receipt IS NOT NULL) AND (receipt_ref IS NOT NULL) AND (receipt_ref <> ''::text)) OR ((receipt IS NULL) AND (receipt_ref IS NULL)))),
    CONSTRAINT outbound_intents_satellite_names_parent CHECK (((target_kind = ANY (ARRAY['thread'::text, 'thread_reply'::text])) = (parent_intent_id IS NOT NULL))),
    CONSTRAINT outbound_intents_sending_has_attempt CHECK (((current_attempt_id IS NOT NULL) = (status = 'sending'::text))),
    CONSTRAINT outbound_intents_sending_has_lease CHECK (((status <> 'sending'::text) OR (lease_token IS NOT NULL))),
    CONSTRAINT outbound_intents_webhook_payload_addresses_the_target CHECK (((key_kind <> ALL (ARRAY['webhook_event'::text, 'webhook_replay'::text])) OR ((NOT (((payload -> 'target'::text) ->> 'kind'::text) IS DISTINCT FROM target_kind)) AND (NOT (((payload -> 'target'::text) ->> 'ref'::text) IS DISTINCT FROM target_ref))))),
    CONSTRAINT outbound_intents_webhook_provider CHECK (((key_kind <> ALL (ARRAY['webhook_event'::text, 'webhook_replay'::text])) OR (provider = 'webhook'::text))),
    CONSTRAINT outbound_intents_webhook_targets_a_subscriber CHECK (((key_kind <> ALL (ARRAY['webhook_event'::text, 'webhook_replay'::text])) OR (target_kind = 'subscriber'::text))),
    CONSTRAINT outbound_receipt_state CHECK ((((NOT receipt_recorded) AND (receipt IS NULL) AND (receipt_redacted_at IS NULL)) OR (receipt_recorded AND (receipt IS NOT NULL) AND (receipt_redacted_at IS NULL)) OR (receipt_recorded AND (receipt IS NULL) AND (receipt_redacted_at IS NOT NULL))))
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

-- Name: integration_tombstones integration_tombstones_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.integration_tombstones
    ADD CONSTRAINT integration_tombstones_pkey PRIMARY KEY (id);

-- Name: integrations integrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.integrations
    ADD CONSTRAINT integrations_pkey PRIMARY KEY (id);

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

-- Name: outbound_attempt_observations outbound_attempt_observations_attempt_id_observation_kind_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_attempt_observations
    ADD CONSTRAINT outbound_attempt_observations_attempt_id_observation_kind_key UNIQUE (attempt_id, observation_kind);

-- Name: outbound_attempt_observations outbound_attempt_observations_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_attempt_observations
    ADD CONSTRAINT outbound_attempt_observations_pkey PRIMARY KEY (id);

-- Name: outbound_attempts outbound_attempts_intent_id_id; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_attempts
    ADD CONSTRAINT outbound_attempts_intent_id_id UNIQUE (intent_id, id);

-- Name: outbound_attempts outbound_attempts_no; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_attempts
    ADD CONSTRAINT outbound_attempts_no UNIQUE (intent_id, attempt_no);

-- Name: outbound_attempts outbound_attempts_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_attempts
    ADD CONSTRAINT outbound_attempts_pkey PRIMARY KEY (id);

-- Name: outbound_batches outbound_batches_batch_key_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_batches
    ADD CONSTRAINT outbound_batches_batch_key_key UNIQUE (batch_key);

-- Name: outbound_batches outbound_batches_identity; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_batches
    ADD CONSTRAINT outbound_batches_identity UNIQUE (id, key_kind, delivery_family, grammar_version);

-- Name: outbound_batches outbound_batches_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_batches
    ADD CONSTRAINT outbound_batches_pkey PRIMARY KEY (id);

-- Name: outbound_group_snapshots outbound_group_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_group_snapshots
    ADD CONSTRAINT outbound_group_snapshots_pkey PRIMARY KEY (alert_group_id);

-- Name: outbound_intent_events outbound_intent_events_intent_id_seq_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_intent_events
    ADD CONSTRAINT outbound_intent_events_intent_id_seq_key UNIQUE (intent_id, seq);

-- Name: outbound_intent_events outbound_intent_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_intent_events
    ADD CONSTRAINT outbound_intent_events_pkey PRIMARY KEY (id);

-- Name: outbound_intents outbound_intents_idempotency_key_key; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_intents
    ADD CONSTRAINT outbound_intents_idempotency_key_key UNIQUE (idempotency_key);

-- Name: outbound_intents outbound_intents_payload_addresses_the_target_v2; Type: CHECK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE public.outbound_intents
    ADD CONSTRAINT outbound_intents_payload_addresses_the_target_v2 CHECK (((key_kind <> ALL (ARRAY['escalation'::text, 'escalation_replay'::text])) OR (payload_schema_version <> ALL (ARRAY[1, 2])) OR ((NOT ((payload #>> '{target,kind}'::text[]) IS DISTINCT FROM target_kind)) AND (NOT ((payload #>> '{target,ref}'::text[]) IS DISTINCT FROM target_ref))))) NOT VALID;

-- Name: outbound_intents outbound_intents_pkey; Type: CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_intents
    ADD CONSTRAINT outbound_intents_pkey PRIMARY KEY (id);

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

CREATE UNIQUE INDEX idx_active_alert_groups ON public.alert_groups USING btree (alert_key) WHERE (status <> ALL (ARRAY['resolved'::text, 'closed'::text]));

-- Name: idx_alert_groups_status; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_alert_groups_status ON public.alert_groups USING btree (status);

-- Name: idx_alert_groups_team_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_alert_groups_team_id ON public.alert_groups USING btree (team_id);

-- Name: idx_api_tokens_hash; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_api_tokens_hash ON public.api_tokens USING btree (token_hash);

-- Name: idx_api_tokens_user; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_api_tokens_user ON public.api_tokens USING btree (user_id);

-- Name: idx_escalation_steps_policy_id; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_escalation_steps_policy_id ON public.escalation_steps USING btree (policy_id);

-- Name: idx_event_outbox_retention; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_event_outbox_retention ON public.event_outbox USING btree (fanned_out_at, id) WHERE (status = ANY (ARRAY['fanned_out'::text, 'completed'::text, 'failed'::text]));

-- Name: idx_external_identities_provider_external; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_external_identities_provider_external ON public.external_identities USING btree (provider, external_id);

-- Name: idx_integrations_type_outbound; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_integrations_type_outbound ON public.integrations USING btree (type) WHERE ((direction = 'outbound'::text) AND (type <> 'generic_webhook'::text));

-- Name: idx_outbound_batches_event; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_batches_event ON public.outbound_batches USING btree (event_id) WHERE (event_id IS NOT NULL);

-- Name: idx_outbound_batches_group_admission; Type: INDEX; Schema: public; Owner: -

CREATE UNIQUE INDEX idx_outbound_batches_group_admission ON public.outbound_batches USING btree (alert_group_id) WHERE ((alert_group_id IS NOT NULL) AND (key_kind = 'escalation'::text));

-- Name: idx_outbound_batches_no_targets; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_batches_no_targets ON public.outbound_batches USING btree (delivery_family) WHERE (admission_outcome = 'no_targets'::text);

-- Name: idx_outbound_intents_batch; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_batch ON public.outbound_intents USING btree (batch_id);

-- Name: idx_outbound_intents_claim; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_claim ON public.outbound_intents USING btree (delivery_family, provider, ((attempts_in_generation = 0)), next_attempt_at, id) WHERE (status = 'pending'::text);

-- Name: idx_outbound_intents_expiring; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_expiring ON public.outbound_intents USING btree (delivery_family, expires_at) WHERE ((status = 'pending'::text) AND (expires_at IS NOT NULL));

-- Name: idx_outbound_intents_first_attempt; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_first_attempt ON public.outbound_intents USING btree (delivery_family, provider, next_attempt_at, id) WHERE ((status = 'pending'::text) AND (attempts_in_generation = 0));

-- Name: idx_outbound_intents_group; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_group ON public.outbound_intents USING btree (alert_group_id) WHERE (alert_group_id IS NOT NULL);

-- Name: idx_outbound_intents_journal; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_journal ON public.outbound_intents USING btree (created_at DESC, id DESC);

-- Name: idx_outbound_intents_parent; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_parent ON public.outbound_intents USING btree (parent_intent_id) WHERE (parent_intent_id IS NOT NULL);

-- Name: idx_outbound_intents_retention; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_retention ON public.outbound_intents USING btree (updated_at, id) WHERE (status = ANY (ARRAY['succeeded'::text, 'permanent_failed'::text, 'expired'::text, 'canceled'::text]));

-- Name: idx_outbound_intents_stale; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_stale ON public.outbound_intents USING btree (delivery_family, locked_until) WHERE (status = 'sending'::text);

-- Name: idx_outbound_intents_status; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_status ON public.outbound_intents USING btree (delivery_family, status);

-- Name: idx_outbound_intents_subscriber; Type: INDEX; Schema: public; Owner: -

CREATE INDEX idx_outbound_intents_subscriber ON public.outbound_intents USING btree (target_ref, created_at DESC) WHERE (delivery_family = 'webhook'::text);

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

-- Name: external_identities external_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.external_identities
    ADD CONSTRAINT external_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

-- Name: integrations integrations_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.integrations
    ADD CONSTRAINT integrations_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id);

-- Name: link_tokens link_tokens_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.link_tokens
    ADD CONSTRAINT link_tokens_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

-- Name: outbound_attempt_observations outbound_attempt_observations_attempt_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_attempt_observations
    ADD CONSTRAINT outbound_attempt_observations_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES public.outbound_attempts(id);

-- Name: outbound_attempts outbound_attempts_intent_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_attempts
    ADD CONSTRAINT outbound_attempts_intent_id_fkey FOREIGN KEY (intent_id) REFERENCES public.outbound_intents(id);

-- Name: outbound_batches outbound_batches_alert_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_batches
    ADD CONSTRAINT outbound_batches_alert_group_id_fkey FOREIGN KEY (alert_group_id) REFERENCES public.alert_groups(id);

-- Name: outbound_group_snapshots outbound_group_snapshots_alert_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_group_snapshots
    ADD CONSTRAINT outbound_group_snapshots_alert_group_id_fkey FOREIGN KEY (alert_group_id) REFERENCES public.alert_groups(id);

-- Name: outbound_intent_events outbound_intent_events_intent_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_intent_events
    ADD CONSTRAINT outbound_intent_events_intent_id_fkey FOREIGN KEY (intent_id) REFERENCES public.outbound_intents(id);

-- Name: outbound_intents outbound_intents_alert_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_intents
    ADD CONSTRAINT outbound_intents_alert_group_id_fkey FOREIGN KEY (alert_group_id) REFERENCES public.alert_groups(id);

-- Name: outbound_intents outbound_intents_batch_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_intents
    ADD CONSTRAINT outbound_intents_batch_id_fkey FOREIGN KEY (batch_id) REFERENCES public.outbound_batches(id);

-- Name: outbound_intents outbound_intents_batch_identity_fk; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_intents
    ADD CONSTRAINT outbound_intents_batch_identity_fk FOREIGN KEY (batch_id, key_kind, delivery_family, grammar_version) REFERENCES public.outbound_batches(id, key_kind, delivery_family, grammar_version);

-- Name: outbound_intents outbound_intents_current_attempt_fk; Type: FK CONSTRAINT; Schema: public; Owner: -

ALTER TABLE ONLY public.outbound_intents
    ADD CONSTRAINT outbound_intents_current_attempt_fk FOREIGN KEY (id, current_attempt_id) REFERENCES public.outbound_attempts(intent_id, id);

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
