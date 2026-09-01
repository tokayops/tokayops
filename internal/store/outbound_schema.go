package store

import (
	"context"
	"fmt"
)

// The outbound delivery schema: where a commitment to send something outside
// lives, from the moment it is accepted until its last attempt is closed.
//
// Six tables, and the split between them is the contract rather than a layout
// preference:
//
//   - outbound_batches is the admission claim. One row per accepted set, and
//     the row is what makes admission idempotent: a repeat of the same set is a
//     no-op, a repeat with different content is a conflict reported to the
//     producer rather than a merged audience.
//   - outbound_group_snapshots holds the state a CARD is rendered from, one row
//     per alert group, superseded forward: bringing a message up to date is
//     what an editable one is for. A one-shot message renders the state its
//     admission froze instead - kept on the batch, never superseded - because
//     one provider key names one external effect, and a retry that followed the
//     group would put two different requests under it. Neither reads the live
//     group.
//   - outbound_intents is one commitment to one recipient: what to send, where
//     it stands, and who holds the lease over it right now.
//   - outbound_attempts is the journal. It carries two kinds of record: a
//     network attempt, written BEFORE the call and closed by exactly one CAS,
//     and a preparation record, which is the proven refusal of a send that
//     never reached the provider at all.
//   - outbound_attempt_observations carries a result that arrived for an
//     attempt somebody else already closed. It cannot be a column on the
//     attempt: a finished attempt is immutable, and a late result may arrive
//     twice with different content, which has to be a conflict rather than a
//     second truth.
//   - outbound_intent_events is the append-only lifecycle and audit log of the
//     intent itself - cancellation, expiry without an attempt, an operator's
//     decision. None of those has a network call or a transport outcome, so
//     none of them belongs in the attempts table.
//
// The schema is created in full on every start, and a database that already has
// the tables is brought up to it in the same transaction. That part DOES touch
// data: a column added to a populated table has to be filled before it can be
// required, and a rule added to one has to be checked against the rows that are
// already there. Those steps live beside the columns they are about, and a row
// none of them can read stops the start rather than being worked around.

// outboundSchemaAdvisoryLock serializes the block below across instances.
//
// The same reason the job dedup model takes one: `CREATE TABLE IF NOT EXISTS`
// is not atomic against a concurrent creator, and two instances starting
// together collide inside the catalog rather than on the table name - an error
// neither of them can read as "somebody else got there first". The number is
// arbitrary and only has to stay stable and distinct from the other lock this
// package takes.
const outboundSchemaAdvisoryLock int64 = 0x746F6B6112

// outboundCurrentAttemptFK is added by its own statement because a foreign key
// cannot be declared inline here: the two tables point at each other. An intent
// names the attempt it is executing right now, and an attempt names the intent
// it belongs to.
const outboundCurrentAttemptFK = "outbound_intents_current_attempt_fk"

const outboundSchemaDDL = `
CREATE TABLE IF NOT EXISTS outbound_batches (
	id                  TEXT PRIMARY KEY,
	-- The admission claim itself. This row is never deleted: retention has to
	-- outlive the deliveries it admitted, or the same work is admitted twice.
	batch_key           TEXT NOT NULL UNIQUE,
	key_kind            TEXT NOT NULL,
	delivery_family     TEXT NOT NULL,
	grammar_version     INT  NOT NULL,
	-- Which alert group this admission belongs to, asked by the producer's own
	-- query. NULL for the families that have no group.
	alert_group_id      TEXT REFERENCES alert_groups(id),
	fingerprint         BYTEA NOT NULL,
	fingerprint_version INT  NOT NULL,
	-- The outcome of admission, stated rather than inferred from a counter:
	-- "nobody to notify" is an answer, and reading it out of intent_count = 0
	-- is reading meaning out of an absence.
	admission_outcome   TEXT NOT NULL,
	intent_count        INT  NOT NULL,
	-- The state this set was admitted from, frozen and never superseded.
	--
	-- The group's own snapshot moves forward: an editable card is brought to
	-- the latest revision, which is the whole point of it. A one-shot message
	-- is not. It is one external effect under one provider key, and a retry of
	-- it has to carry the bytes the admission accepted - otherwise the same key
	-- names two different requests, and a provider that lost its answer to the
	-- first receives the second under the identity of the first.
	--
	-- Every commitment of a batch shares this, because they were admitted from
	-- one reading of one moment: that is what the batch IS.
	admission_snapshot  JSONB,
	admission_digest    BYTEA,
	admission_schema_version INT,
	admission_revision  BIGINT,
	admitted_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT outbound_batches_outcome_known
		CHECK (admission_outcome IN ('admitted', 'no_targets')),
	-- Stated in both directions, so neither half can drift from the other.
	CONSTRAINT outbound_batches_outcome_shape
		CHECK ((admission_outcome = 'no_targets') = (intent_count = 0)),
	CONSTRAINT outbound_batches_count_nonneg CHECK (intent_count >= 0),
	-- The fingerprint is a SHA-256 digest, so NOT NULL is only half the rule:
	-- an empty bytea is a value, and a producer that computed nothing would
	-- otherwise match every other producer that computed nothing.
	CONSTRAINT outbound_batches_fingerprint_len
		CHECK (octet_length(fingerprint) = 32)
	-- Two more rules about this table - that the frozen state is whole, and
	-- that an escalation has one - are added by their own statements below, so
	-- that they reach databases created before those columns existed.
);

-- One FIRST admission per group. Scoped to that kind on purpose: a later
-- re-admission of the same group is a different kind of claim, and a rule
-- written over every kind would make one structurally impossible.
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbound_batches_group_admission
	ON outbound_batches (alert_group_id)
	WHERE alert_group_id IS NOT NULL AND key_kind = 'escalation';

CREATE TABLE IF NOT EXISTS outbound_group_snapshots (
	alert_group_id          TEXT PRIMARY KEY REFERENCES alert_groups(id),
	revision                BIGINT NOT NULL,
	snapshot_schema_version INT    NOT NULL,
	snapshot                JSONB  NOT NULL,
	-- The canonical digest of the snapshot above. It is what tells two
	-- proposals for the same revision apart - without it, a second producer
	-- offering different content would be answered "already accepted" and its
	-- content would vanish silently. One version covers both the stored shape
	-- and the digest, because the codec is defined over the same fields and a
	-- second counter would eventually disagree with the first.
	snapshot_digest         BYTEA  NOT NULL,
	final                   BOOLEAN NOT NULL DEFAULT FALSE,
	updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT outbound_group_snapshots_revision CHECK (revision >= 0),
	CONSTRAINT outbound_group_snapshots_digest_len
		CHECK (octet_length(snapshot_digest) = 32)
);

CREATE TABLE IF NOT EXISTS outbound_intents (
	id                         TEXT PRIMARY KEY,
	batch_id                   TEXT NOT NULL REFERENCES outbound_batches(id),
	idempotency_key            TEXT NOT NULL UNIQUE,
	-- The execution partition. Claims are taken per family so a backlog of one
	-- kind of delivery cannot delay another.
	delivery_family            TEXT NOT NULL,
	key_kind                   TEXT NOT NULL,
	grammar_version            INT  NOT NULL,
	provider                   TEXT NOT NULL,
	target_kind                TEXT NOT NULL,
	-- The recipient as this system names them, not as the provider does. The
	-- provider's own address is bound to the generation below.
	target_ref                 TEXT NOT NULL,
	alert_group_id             TEXT REFERENCES alert_groups(id),
	form                       TEXT NOT NULL,
	completion_mode            TEXT NOT NULL,
	ambiguity_policy           TEXT NOT NULL,
	payload_schema_version     INT  NOT NULL,
	payload                    JSONB NOT NULL,
	provider_key_codec_version INT  NOT NULL,

	status                     TEXT NOT NULL,
	generation_no              INT  NOT NULL DEFAULT 0,
	-- How many journal records this generation already has. It answers exactly
	-- one question - has this generation been tried at all - which is what
	-- lets a freshly admitted delivery be claimed ahead of an old retry.
	attempts_in_generation     INT  NOT NULL DEFAULT 0,
	-- The current run of failures, and the step of the backoff curve. Reset by
	-- any success, because the failures of an effect that has already happened
	-- have no business slowing down the next one.
	failure_streak             INT  NOT NULL DEFAULT 0,
	bound_endpoint             TEXT,
	create_key                 TEXT,
	-- The coordinates of the external object, and - separately - the FACT that
	-- one exists. The two are separate because erasure removes the first and
	-- must not remove the second: a message that was sent stays sent, and the
	-- state machine reads "is there something out there" from receipt_recorded,
	-- never from the coordinates themselves.
	receipt                    JSONB,
	-- The name the external object is known by, as the channel that made it
	-- spells one. It is kept beside the coordinates so that the domain can say
	-- WHICH object a change is aimed at without reading a provider's JSON:
	-- parsing that here would put Slack's field names, and Telegram's, inside
	-- rules that are supposed to hold for every channel there will ever be.
	receipt_ref                TEXT,
	-- What this commitment's payload was when it was admitted, canonicalised.
	-- Every attempt recomputes it from the payload on the row and compares:
	-- without it there is nothing to compare against, because the payload is
	-- not in the business key and its wire form reaches only the intent
	-- fingerprint, which an attempt does not recompute.
	payload_digest             BYTEA NOT NULL,
	receipt_recorded           BOOLEAN NOT NULL DEFAULT FALSE,
	receipt_redacted_at        TIMESTAMPTZ,
	-- When the recipient of this commitment was erased. A durable prohibition
	-- rather than a record: every writer after it must refuse to put personal
	-- coordinates back.
	recipient_erased_at        TIMESTAMPTZ,
	desired_revision           BIGINT NOT NULL DEFAULT 0,
	applied_revision           BIGINT,
	final_revision_applied     BOOLEAN NOT NULL DEFAULT FALSE,
	cancellation_requested     BOOLEAN NOT NULL DEFAULT FALSE,
	accepted_duplicate_risk    BOOLEAN NOT NULL DEFAULT FALSE,

	not_before                 TIMESTAMPTZ NOT NULL,
	next_attempt_at            TIMESTAMPTZ NOT NULL,
	expires_at                 TIMESTAMPTZ,
	-- Reserved for the channels whose acceptance only means "queued". No
	-- channel here works that way yet, and nothing reads this column; it is in
	-- the schema so the state machine that will need it does not have to be
	-- rebuilt around it.
	receipt_timeout_at         TIMESTAMPTZ,

	current_attempt_id         TEXT,
	lease_token                TEXT,
	locked_until               TIMESTAMPTZ,
	-- Audit only. The lease is the token above; a worker's identity never
	-- authorises anything.
	worker_id                  TEXT,

	created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),

	-- "Sending" and "has an attempt in flight" are one fact, so they are one
	-- statement. Split, they drift, and a row that looks busy while nothing is
	-- running is a delivery nobody will ever finish.
	CONSTRAINT outbound_intents_sending_has_attempt
		CHECK ((current_attempt_id IS NOT NULL) = (status = 'sending')),
	-- Releasing a lease clears both halves. One without the other leaves the
	-- row unselectable until a lease nobody holds expires.
	CONSTRAINT outbound_intents_lease_pair
		CHECK ((lease_token IS NULL) = (locked_until IS NULL)),
	-- The cancellation flag lives on a send in flight and is consumed when that
	-- send finishes. Anywhere else it is a decision with nothing to apply it to.
	CONSTRAINT outbound_intents_cancel_flag_on_sending
		CHECK (NOT cancellation_requested OR status = 'sending'),
	-- A lease belongs to work that is claimable or running. A terminal row
	-- holding one is not a state, it is a bug that hides the row forever.
	CONSTRAINT outbound_intents_lease_only_when_working
		CHECK (lease_token IS NULL OR status IN ('pending', 'sending')),
	CONSTRAINT outbound_intents_sending_has_lease
		CHECK (status <> 'sending' OR lease_token IS NOT NULL),
	CONSTRAINT outbound_intents_counters_nonneg
		CHECK (attempts_in_generation >= 0 AND failure_streak >= 0
			AND generation_no >= 0)
	-- One more rule about this table - that a commitment names its recipient
	-- one way - is added by its own statement below, so that it reaches
	-- databases created before it was written.
);

CREATE TABLE IF NOT EXISTS outbound_attempts (
	id                            TEXT PRIMARY KEY,
	intent_id                     TEXT NOT NULL REFERENCES outbound_intents(id),
	-- Monotonic within the intent. "The last attempt" is decided by this and
	-- not by created_at: two records written in one transaction share a
	-- timestamp, and an order that is not total is not an order.
	attempt_no                    INT  NOT NULL,
	record_kind                   TEXT NOT NULL,
	generation_no                 INT  NOT NULL,
	attempt_kind                  TEXT NOT NULL,
	operation                     TEXT NOT NULL,
	applied_revision              BIGINT,
	provider                      TEXT NOT NULL,
	bound_endpoint                TEXT,
	provider_key                  TEXT,
	request_fingerprint           BYTEA,
	-- The fencing token of the worker that owns this attempt. It is kept here,
	-- on a row nothing rewrites, because the intent clears its own token the
	-- moment the attempt is finalised.
	lease_token                   TEXT,
	worker_id                     TEXT,
	started_at                    TIMESTAMPTZ,
	finished_at                   TIMESTAMPTZ,
	outcome                       TEXT,
	error_class                   TEXT,
	provider_status               TEXT,
	-- The coordinates this attempt obtained. They stay here after the intent
	-- moves on to a new external effect and clears its own copy - otherwise the
	-- address of a message that was actually sent is lost.
	receipt                       JSONB,
	receipt_recorded              BOOLEAN NOT NULL DEFAULT FALSE,
	receipt_redacted_at           TIMESTAMPTZ,
	-- A short, safe summary of the response. Never the payload, never secrets.
	-- It can still name an address - "accepted with channel=D0123" - so erasure
	-- clears it along with the coordinates.
	response_summary              TEXT,
	finish_reason                 TEXT,
	-- What the answer PROVED about the object, where it proved anything. It
	-- exists only at the moment of the attempt: an operator deciding weeks
	-- later whether a second message may be made has nothing else to read.
	provider_result_detail        TEXT,
	completion_fingerprint        BYTEA,
	-- Fixed when the attempt starts rather than when it finishes: an attempt
	-- can outlive a deployment, and the encoder that closes it has to be the
	-- one that opened it.
	completion_fingerprint_version INT,
	created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),

	CONSTRAINT outbound_attempts_no UNIQUE (intent_id, attempt_no),
	-- The target of the intent's composite foreign key: an intent may only
	-- point at an attempt that is its own.
	CONSTRAINT outbound_attempts_intent_id_id UNIQUE (intent_id, id),
	-- The two record kinds are different shapes, and the difference is the
	-- whole point: a started attempt means the network MIGHT have been called,
	-- while a preparation record is the proof that it was not.
	CONSTRAINT outbound_attempts_kind_shape CHECK (
		(record_kind = 'attempt'
			AND started_at IS NOT NULL AND lease_token IS NOT NULL)
		OR
		(record_kind = 'preparation'
			AND started_at IS NULL AND lease_token IS NULL
			AND finished_at IS NOT NULL AND finish_reason = 'preparation'
			-- A preparation record is not closed by a compare-and-set, so it
			-- has nothing to compare: a fingerprint here would be a repeat
			-- check for a repeat that cannot happen.
			AND completion_fingerprint IS NULL
			AND completion_fingerprint_version IS NULL)
	),
	CONSTRAINT outbound_attempts_finished_shape CHECK (
		(finished_at IS NULL AND outcome IS NULL AND finish_reason IS NULL)
		OR
		(finished_at IS NOT NULL AND outcome IS NOT NULL AND finish_reason IS NOT NULL
			AND (started_at IS NULL OR finished_at >= started_at))
	),
	-- A network attempt carries the version from the moment it starts, and its
	-- fingerprint from the moment it ends. Without both, a repeated finalise
	-- after a lost commit reply cannot be told from a contradicting one.
	CONSTRAINT outbound_attempts_fingerprint_shape CHECK (
		record_kind <> 'attempt'
		OR (completion_fingerprint_version IS NOT NULL
			AND (finished_at IS NULL) = (completion_fingerprint IS NULL))
	),
	-- The completion fingerprint is a declared hash protocol, so its length is
	-- part of the contract. request_fingerprint is not one: it is an opaque
	-- audit value with no domain separator and no version of its own, and
	-- pinning a length here would assert a protocol nobody wrote.
	CONSTRAINT outbound_attempts_digest_len CHECK (
		completion_fingerprint IS NULL OR octet_length(completion_fingerprint) = 32
	)
);

CREATE TABLE IF NOT EXISTS outbound_attempt_observations (
	id                            TEXT PRIMARY KEY,
	attempt_id                    TEXT NOT NULL REFERENCES outbound_attempts(id),
	observation_kind              TEXT NOT NULL,
	outcome                       TEXT NOT NULL,
	error_class                   TEXT,
	provider_status               TEXT,
	-- A late acceptance is sometimes the only proof that the effect happened,
	-- so what is kept is the result, not just a hash of it.
	receipt                       JSONB,
	receipt_recorded              BOOLEAN NOT NULL DEFAULT FALSE,
	receipt_redacted_at           TIMESTAMPTZ,
	applied_revision              BIGINT,
	provider_result_detail        TEXT,
	response_summary              TEXT,
	completion_fingerprint        BYTEA NOT NULL,
	completion_fingerprint_version INT  NOT NULL,
	observed_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
	-- Identity is the attempt and the kind; the fingerprint is content. Two
	-- contradicting late results for one attempt are a conflict, not two rows.
	UNIQUE (attempt_id, observation_kind),
	CONSTRAINT outbound_attempt_observations_digest_len
		CHECK (octet_length(completion_fingerprint) = 32)
);

CREATE TABLE IF NOT EXISTS outbound_intent_events (
	id             TEXT PRIMARY KEY,
	intent_id      TEXT NOT NULL REFERENCES outbound_intents(id),
	seq            INT  NOT NULL,
	kind           TEXT NOT NULL,
	reason         TEXT,
	actor          TEXT,
	from_status    TEXT,
	to_status      TEXT,
	generation_no  INT,
	detail         JSONB,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (intent_id, seq)
);

-- Claims: by family and provider, oldest due first. The provider is in the
-- predicate because the pool is shared and one provider's backlog must not be
-- able to take every slot.
-- The order is the claim's own: already attempted first (FALSE sorts before
-- TRUE), then oldest due. The expression is in the index because the claim
-- sorts by it, and without that PostgreSQL has to read and sort every due row
-- of the family to answer a LIMIT of four - which is fine until the outage that
-- makes the queue long, and useless exactly then.
CREATE INDEX IF NOT EXISTS idx_outbound_intents_claim
	ON outbound_intents
		(delivery_family, provider, (attempts_in_generation = 0), next_attempt_at, id)
	WHERE status = 'pending';
-- Superseded by the index above, which has the same leading columns.
DROP INDEX IF EXISTS idx_outbound_intents_due;
-- The same claim, restricted to deliveries nobody has tried yet, so a new page
-- does not queue behind a pile of old retries.
CREATE INDEX IF NOT EXISTS idx_outbound_intents_first_attempt
	ON outbound_intents (delivery_family, provider, next_attempt_at, id)
	WHERE status = 'pending' AND attempts_in_generation = 0;
CREATE INDEX IF NOT EXISTS idx_outbound_intents_expiring
	ON outbound_intents (delivery_family, expires_at)
	WHERE status = 'pending' AND expires_at IS NOT NULL;
-- Recovery: rows whose lease died while an attempt was in flight.
CREATE INDEX IF NOT EXISTS idx_outbound_intents_stale
	ON outbound_intents (delivery_family, locked_until)
	WHERE status = 'sending';
CREATE INDEX IF NOT EXISTS idx_outbound_intents_group
	ON outbound_intents (alert_group_id)
	WHERE alert_group_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_outbound_intents_status
	ON outbound_intents (delivery_family, status);
-- The delivery history of one subscriber, newest first: what the webhook API
-- lists per integration. The family is in the predicate because target_ref is
-- a user id for every other family, and this index has nothing to say there.
CREATE INDEX IF NOT EXISTS idx_outbound_intents_subscriber
	ON outbound_intents (target_ref, created_at DESC)
	WHERE delivery_family = 'webhook';

-- The journal is read by (intent, number) and by (intent, sequence), and both
-- of those already have an index: the unique constraints that make those pairs
-- identities in the first place. A second index over the same columns would be
-- paid for on every write and read by nothing.
`

// outboundCurrentAttemptFKDDL adds the half of the cycle that cannot be
// declared with its table.
//
// Postgres has no IF NOT EXISTS for ADD CONSTRAINT, and this runs on every
// start, so the catalog is asked first - by name AND by table, because
// constraint names are unique per table and a namesake on another one would
// otherwise read as "already there". The key is composite - (id,
// current_attempt_id) against (intent_id, id) - rather than a plain reference
// to the attempt's primary key: a single-column key would happily let an intent
// name somebody else's attempt, and then "the attempt this intent is executing"
// would be held by code alone.
const outboundCurrentAttemptFKDDL = `
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + outboundCurrentAttemptFK + `'
		  AND conrelid = 'outbound_intents'::regclass
	) THEN
		ALTER TABLE outbound_intents
			ADD CONSTRAINT ` + outboundCurrentAttemptFK + `
			FOREIGN KEY (id, current_attempt_id)
			REFERENCES outbound_attempts (intent_id, id);
	END IF;
END $$;
`

// outboundTargetAgreementConstraint is the name of the rule below.
// The erasure columns, for databases created before they existed, plus the rule
// that keeps the three receipt states apart.
//
// A receipt is in one of three states, and the difference between the last two
// is the whole point of erasure being a prohibition rather than a delete:
//
//	none      no external object is proved to exist
//	usable    it exists and these are its coordinates
//	redacted  it exists and its coordinates have been removed
//
// Written as a CHECK because the alternative is three columns that can disagree,
// and a row claiming both "no receipt" and "redacted" would be a row nobody can
// interpret afterwards.
const outboundReceiptStateConstraint = "outbound_receipt_state"

func outboundReceiptStateDDL(table string) string {
	return `
DO $$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM information_schema.columns
	               WHERE table_name = '` + table + `' AND column_name = 'receipt_recorded') THEN
		ALTER TABLE ` + table + ` ADD COLUMN receipt_recorded BOOLEAN NOT NULL DEFAULT FALSE;
		ALTER TABLE ` + table + ` ADD COLUMN receipt_redacted_at TIMESTAMPTZ;
		-- Every row written before this column existed carries its coordinates,
		-- so the fact and the coordinates agree by construction.
		UPDATE ` + table + ` SET receipt_recorded = TRUE WHERE receipt IS NOT NULL;
	END IF;

	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + outboundReceiptStateConstraint + `'
		  AND conrelid = '` + table + `'::regclass
	) THEN
		ALTER TABLE ` + table + `
			ADD CONSTRAINT ` + outboundReceiptStateConstraint + ` CHECK (
				(NOT receipt_recorded AND receipt IS NULL AND receipt_redacted_at IS NULL)
				OR (receipt_recorded AND receipt IS NOT NULL AND receipt_redacted_at IS NULL)
				OR (receipt_recorded AND receipt IS NULL AND receipt_redacted_at IS NOT NULL)
			) NOT VALID;
	END IF;

	-- And validated, in a step of its own so that a database which somehow got
	-- the constraint without the backfill fails HERE, loudly, rather than
	-- carrying rows nobody can interpret. NOT VALID on its own only promises
	-- something about rows written from now on, and the whole point of these
	-- three states is that every row has exactly one of them.
	IF EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + outboundReceiptStateConstraint + `'
		  AND conrelid = '` + table + `'::regclass
		  AND NOT convalidated
	) THEN
		ALTER TABLE ` + table + ` VALIDATE CONSTRAINT ` + outboundReceiptStateConstraint + `;
	END IF;
END $$;
`
}

// outboundRecipientErasedDDL adds the durable prohibition itself.
const outboundRecipientErasedDDL = `
DO $$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM information_schema.columns
	               WHERE table_name = 'outbound_intents' AND column_name = 'recipient_erased_at') THEN
		ALTER TABLE outbound_intents ADD COLUMN recipient_erased_at TIMESTAMPTZ;
	END IF;
END $$;
`

const outboundAdmittedStateShape = "outbound_batches_admission_snapshot_shape"

// outboundAdmittedStateShapeDDL says the frozen state is whole or absent.
//
// Four columns describing one moment: a row holding some of them describes a
// moment nobody can reconstruct, and the reader that meets it can only guess
// which half is missing. Families with no render snapshot - handoff, webhook -
// hold none of the four.
const outboundAdmittedStateShapeDDL = `
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + outboundAdmittedStateShape + `'
		  AND conrelid = 'outbound_batches'::regclass
	) THEN
		ALTER TABLE outbound_batches
			ADD CONSTRAINT ` + outboundAdmittedStateShape + ` CHECK (
				(admission_snapshot IS NULL AND admission_digest IS NULL
					AND admission_schema_version IS NULL AND admission_revision IS NULL)
				OR
				(admission_snapshot IS NOT NULL AND admission_digest IS NOT NULL
					AND admission_schema_version IS NOT NULL
					AND admission_revision IS NOT NULL
					AND octet_length(admission_digest) = 32
					AND admission_revision >= 0)
			);
	END IF;
END $$;
`

const outboundReceiptNameConstraint = "outbound_intents_receipt_is_named"

// outboundReceiptRefDDL adds the object's name beside its coordinates, and
// insists the two agree.
//
// A usable receipt without a name is not a smaller problem than no receipt at
// all: the first change aimed at it is refused for coordinates that are
// perfectly good, and the card stops being brought up to date for a reason
// nobody can see from the row.
//
// Nothing can supply the name after the fact - it is the channel's own spelling
// of what it made, and this build does not know which channel wrote a row it
// did not write. So a database holding one stops the start and says what to do
// about it, exactly like the snapshots that predate the protocol. Nothing is
// deployed from that version anywhere, so a local reset is the honest answer.
const outboundReceiptRefDDL = `
DO $$
DECLARE
	unnamed INT;
BEGIN
	IF NOT EXISTS (SELECT 1 FROM information_schema.columns
	               WHERE table_name = 'outbound_intents'
	                 AND column_name = 'receipt_ref') THEN
		ALTER TABLE outbound_intents ADD COLUMN receipt_ref TEXT;

		SELECT count(*) INTO unnamed
		FROM outbound_intents
		WHERE receipt_recorded AND receipt IS NOT NULL;

		IF unnamed > 0 THEN
			RAISE EXCEPTION 'this database holds % message(s) whose coordinates were '
				'written before the name beside them existed. Nothing can supply that '
				'name after the fact - it is the channel''s own spelling of what it '
				'made - and the first change aimed at one would be refused. Drop the '
				'outbound_* tables and let this version create them, or start from a '
				'fresh database.', unnamed;
		END IF;
	END IF;

	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + outboundReceiptNameConstraint + `'
		  AND conrelid = 'outbound_intents'::regclass
	) THEN
		ALTER TABLE outbound_intents
			ADD CONSTRAINT ` + outboundReceiptNameConstraint + ` CHECK (
				-- Coordinates and a name arrive together and leave together.
				-- Erasure takes both and leaves the FACT behind, which is the
				-- third state and the one with neither.
				(receipt IS NOT NULL AND receipt_ref IS NOT NULL AND receipt_ref <> '')
				OR (receipt IS NULL AND receipt_ref IS NULL)
			);
	END IF;
END $$;
`

const outboundResultDetailConstraint = "outbound_attempts_result_detail_known"

// outboundResultDetailDDL adds the column that keeps what an answer proved
// about the object, and closes its vocabulary.
//
// The words are reconciliation's, and this build asks no reconciliation
// question - so the only one it ever writes is that the object is gone. The
// rule is on the whole set anyway: the column is read by an operator deciding
// whether a second message may exist, and a value from nowhere read as though
// it were one of these would be read as permission.
const outboundResultDetailDDL = `
DO $$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM information_schema.columns
	               WHERE table_name = 'outbound_attempts'
	                 AND column_name = 'provider_result_detail') THEN
		ALTER TABLE outbound_attempts ADD COLUMN provider_result_detail TEXT;
	END IF;

	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + outboundResultDetailConstraint + `'
		  AND conrelid = 'outbound_attempts'::regclass
	) THEN
		ALTER TABLE outbound_attempts
			ADD CONSTRAINT ` + outboundResultDetailConstraint + ` CHECK (
				provider_result_detail IS NULL
				OR provider_result_detail IN ('acceptance_proven', 'delivery_proven',
					'definitely_absent', 'inconclusive')
			);
	END IF;
END $$;
`

const outboundFormKnownConstraint = "outbound_intents_form_known"

// outboundFormKnownDDL closes the set of forms in the database as well as in
// the code.
//
// The form decides which state an attempt renders - a card follows the alert, a
// message keeps what its admission froze - so a third value would have to be
// silently treated as one of the two. The code refuses it before an attempt
// exists; this makes the row unwritable in the first place.
const outboundFormKnownDDL = `
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + outboundFormKnownConstraint + `'
		  AND conrelid = 'outbound_intents'::regclass
	) THEN
		ALTER TABLE outbound_intents
			ADD CONSTRAINT ` + outboundFormKnownConstraint + `
			CHECK (form IN ('editable', 'one_shot'));
	END IF;
END $$;
`

const outboundAdmittedStateConstraint = "outbound_batches_admission_snapshot_present"

// outboundAdmittedStateDDL brings the frozen admission state to databases the
// previous version created, and decides what to do about the claims in them.
//
// The columns cannot arrive with the table: it is created by CREATE TABLE IF
// NOT EXISTS, so on a database that already has one they would never appear -
// and the first admission afterwards would fail on a column that is not there.
// That part is structural and applies to every database.
//
// What to do with the claims already there is a different question, and the
// answer is deliberately narrow.
//
// A snapshot written before 2026-08-25 carries the alert's history under tag
// 14. Copying such a row into the batch would produce a claim that parses as
// nothing this build can read - the codec refuses fields it does not know - and
// the commitment under it would end as undeliverable at the moment somebody
// needed it. Repairing it is not possible either: the digest those commitments
// were keyed against covered a field this protocol no longer has.
//
// So the three cases are answered separately, and none of them by guessing:
//
//   - no claims at all: the structure is brought up to date and there is
//     nothing else to do. This is every database that has not escalated yet;
//   - claims whose snapshots are in the current format: filled in from the
//     group's own snapshot at revision 0, which IS what they were admitted
//     from - nothing that wrote them could move a revision off zero;
//   - claims whose snapshots still carry the history: the start stops and says
//     so. This build cannot deliver those commitments, and pretending
//     otherwise would hide it until a page failed. TokayOps is not deployed
//     from that version anywhere, so the remedy is a local reset rather than a
//     migration nobody would ever run twice.
const outboundAdmittedStateDDL = `
DO $$
DECLARE
	stale INT;
	orphaned INT;
BEGIN
	IF NOT EXISTS (SELECT 1 FROM information_schema.columns
	               WHERE table_name = 'outbound_batches'
	                 AND column_name = 'admission_snapshot') THEN

		SELECT count(*) INTO stale
		FROM outbound_group_snapshots
		WHERE snapshot ? 'timeline';

		IF stale > 0 THEN
			RAISE EXCEPTION 'this database holds % render snapshot(s) written before '
				'2026-08-25, when the alert history left the protocol. The deliveries '
				'admitted from them cannot be rendered by this build and cannot be '
				'repaired: the digest they were keyed against covered a field that no '
				'longer exists. Drop the outbound_* tables and let this version create '
				'them, or start from a fresh database.', stale;
		END IF;

		ALTER TABLE outbound_batches
			ADD COLUMN admission_snapshot JSONB,
			ADD COLUMN admission_digest BYTEA,
			ADD COLUMN admission_schema_version INT,
			ADD COLUMN admission_revision BIGINT;

		UPDATE outbound_batches b
		SET admission_snapshot = g.snapshot,
		    admission_digest = g.snapshot_digest,
		    admission_schema_version = g.snapshot_schema_version,
		    admission_revision = g.revision
		FROM outbound_group_snapshots g
		WHERE g.alert_group_id = b.alert_group_id
		  AND b.admission_snapshot IS NULL
		  AND g.revision = 0;

		SELECT count(*) INTO orphaned
		FROM outbound_batches
		WHERE admission_snapshot IS NULL
		  AND key_kind IN ('escalation', 'escalation_replay');

		IF orphaned > 0 THEN
			RAISE EXCEPTION 'this database holds % escalation claim(s) with no state to '
				'render from, and none can be reconstructed. Drop the outbound_* tables '
				'and let this version create them, or start from a fresh database.',
				orphaned;
		END IF;
	END IF;
END $$;
`

// outboundAdmittedStateConstraintDDL insists that an escalation names the state
// it was admitted from.
//
// Every commitment of such a batch renders from it - a card until the alert
// moves, a direct message forever - so a claim without it admits work that is
// guaranteed to end as unrenderable. Families with no render snapshot are
// exempt by name rather than by silence: handoff and webhook carry their own
// content and have nothing to freeze.
//
// It is added by its own statement, after the backfill above, so that a
// database from the previous version is repaired before the rule is applied to
// it. A row that cannot be repaired fails the ALTER and the start with it,
// which is the loud half of the same decision.
const outboundAdmittedStateConstraintDDL = `
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + outboundAdmittedStateConstraint + `'
		  AND conrelid = 'outbound_batches'::regclass
	) THEN
		ALTER TABLE outbound_batches
			ADD CONSTRAINT ` + outboundAdmittedStateConstraint + ` CHECK (
				key_kind NOT IN ('escalation', 'escalation_replay')
				OR (admission_snapshot IS NOT NULL AND admission_digest IS NOT NULL
					AND admission_schema_version IS NOT NULL
					AND admission_revision IS NOT NULL)
			);
	END IF;
END $$;
`

const outboundTargetAgreementConstraint = "outbound_intents_payload_addresses_the_target"

// outboundTargetAgreementDDL states that a commitment may only name its
// recipient one way.
//
// The recipient is named twice: in the columns, which decide WHERE a message
// goes, and inside the payload, which decides WHAT is written. A row where the
// two disagree does not produce a confusing journal entry - it delivers what
// was composed for one person into the channel named beside it. A channel
// compares them before it sends; this makes the row impossible to write.
//
// Added by its own statement rather than with the table, for the same reason
// the foreign key above is: declared inline it appears only in databases
// created after it was written, and every existing one would keep accepting the
// rows it forbids. Asked by name AND by table, like the other one.
//
// NOT VALID, and that is deliberate. It applies to every row written from now
// on, which is the whole point, and it does not stop the process from starting
// against a database that already holds rows from before the rule existed -
// those are pre-cutover rows that the cutover destroys.
//
// Scoped to the shapes it knows: both escalation kinds share one payload, and
// a later payload schema or another kind brings its own rule rather than being
// silently exempt from this one.
const outboundTargetAgreementDDL = `
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + outboundTargetAgreementConstraint + `'
		  AND conrelid = 'outbound_intents'::regclass
	) THEN
		ALTER TABLE outbound_intents
			ADD CONSTRAINT ` + outboundTargetAgreementConstraint + ` CHECK (
				key_kind NOT IN ('escalation', 'escalation_replay')
				OR payload_schema_version <> 1
				-- IS NOT DISTINCT FROM, not =: a payload with no target at all
				-- yields NULL, and a CHECK that evaluates to NULL is satisfied.
				-- Written with =, the one row that names its recipient only
				-- once would be the one row this rule let through.
				OR (payload #>> '{target,kind}' IS NOT DISTINCT FROM target_kind
					AND payload #>> '{target,ref}' IS NOT DISTINCT FROM target_ref)
			) NOT VALID;
	END IF;
END $$;
`

// applyOutboundSchema creates the outbound delivery schema, in one transaction,
// on every start.
//
// The lock is taken first and the whole block runs inside it, so instances
// starting together queue rather than race: the second one finds everything in
// place and its own statements become no-ops.
func (s *Store) applyOutboundSchema() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, outboundSchemaAdvisoryLock); err != nil {
		return fmt.Errorf("failed to take the outbound schema lock: %w", err)
	}

	if _, err := tx.Exec(outboundSchemaDDL); err != nil {
		return fmt.Errorf("failed to create the outbound delivery schema: %w", err)
	}

	if _, err := tx.Exec(outboundCurrentAttemptFKDDL); err != nil {
		return fmt.Errorf("failed to add %s: %w", outboundCurrentAttemptFK, err)
	}
	if _, err := tx.Exec(outboundTargetAgreementDDL); err != nil {
		return fmt.Errorf("failed to add %s: %w", outboundTargetAgreementConstraint, err)
	}
	if _, err := tx.Exec(outboundRecipientErasedDDL); err != nil {
		return fmt.Errorf("failed to add the erasure marker: %w", err)
	}
	if _, err := tx.Exec(outboundAdmittedStateDDL); err != nil {
		return fmt.Errorf("failed to add the admitted state: %w", err)
	}
	if _, err := tx.Exec(outboundAdmittedStateShapeDDL); err != nil {
		return fmt.Errorf("failed to add %s: %w", outboundAdmittedStateShape, err)
	}
	if _, err := tx.Exec(outboundAdmittedStateConstraintDDL); err != nil {
		return fmt.Errorf("failed to add %s: %w", outboundAdmittedStateConstraint, err)
	}
	if _, err := tx.Exec(outboundFormKnownDDL); err != nil {
		return fmt.Errorf("failed to add %s: %w", outboundFormKnownConstraint, err)
	}
	if _, err := tx.Exec(outboundResultDetailDDL); err != nil {
		return fmt.Errorf("failed to add %s: %w", outboundResultDetailConstraint, err)
	}
	if _, err := tx.Exec(outboundReceiptRefDDL); err != nil {
		return fmt.Errorf("failed to add the receipt's name: %w", err)
	}
	for _, table := range []string{
		"outbound_intents", "outbound_attempts", "outbound_attempt_observations",
	} {
		if _, err := tx.Exec(outboundReceiptStateDDL(table)); err != nil {
			return fmt.Errorf("failed to add %s on %s: %w",
				outboundReceiptStateConstraint, table, err)
		}
	}

	if err := applyPayloadDigest(context.Background(), tx); err != nil {
		return err
	}

	return tx.Commit()
}
