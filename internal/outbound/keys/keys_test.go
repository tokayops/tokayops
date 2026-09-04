package keys

import (
	"errors"
	"strings"
	"testing"
)

// The fixtures below are deliberately dull and fixed: a golden vector is only
// worth having if the input it pins can be read at a glance.
const (
	fixtureGroup    = "ag-0001"
	fixtureProvider = "slack"
	fixtureChannel  = "C0001"
	fixtureRequest  = "req-0001"
)

func fixtureIntent() escalationIntent {
	return escalationIntent{
		AlertGroupID: fixtureGroup,
		Slot:         Slot{Kind: SlotPolicy, Index: 2},
		Provider:     fixtureProvider,
		Target:       Target{Kind: TargetChannel, Ref: fixtureChannel},
	}
}

func mustIntentKey(t *testing.T, i escalationIntent, kind Kind) string {
	t.Helper()
	key, err := i.key(kind, GrammarV1)
	if err != nil {
		t.Fatalf("build the intent key: %v", err)
	}
	return key
}

// TestEscalationKeysAreGolden pins the bytes.
//
// Changing any of these values is changing the identity of every commitment
// and every admission already written: a row keyed under the old spelling
// becomes unreachable, and the work it claimed is admitted a second time. That
// is a decision with a version number attached, not a refactor, and this test
// is where it has to be made deliberately.
func TestEscalationKeysAreGolden(t *testing.T) {
	cases := []struct {
		name string
		got  func(t *testing.T) string
		want string
	}{
		{
			name: "escalation intent",
			got:  func(t *testing.T) string { return mustIntentKey(t, fixtureIntent(), KindEscalation) },
			want: "ag-0001:c2c6d3b045693d3ac9d5b158c88c0ffb1d5183d2f9cbe0b26ca754193fd48c04",
		},
		{
			name: "escalation batch",
			got: func(t *testing.T) string {
				key, err := BatchIdentity{Kind: KindEscalation, AlertGroupID: fixtureGroup}.BatchKey(GrammarV1)
				if err != nil {
					t.Fatalf("build the batch key: %v", err)
				}
				return key
			},
			want: "ag-0001:f87931d8c8231a09dd939f26e47cdb47d9d94c53433edd8c80572adeb7199e53",
		},
		{
			name: "re-admission intent",
			got: func(t *testing.T) string {
				i := fixtureIntent()
				i.ClientRequestID = fixtureRequest
				return mustIntentKey(t, i, KindEscalationReplay)
			},
			want: "ag-0001:a3fa8b02d698e472e099801351ead4cffdd716f2177ff1bccf311fa5bdf361be",
		},
		{
			name: "re-admission batch",
			got: func(t *testing.T) string {
				key, err := BatchIdentity{
					Kind: KindEscalationReplay, AlertGroupID: fixtureGroup,
					ClientRequestID: fixtureRequest,
				}.BatchKey(GrammarV1)
				if err != nil {
					t.Fatalf("build the batch key: %v", err)
				}
				return key
			},
			want: "ag-0001:5c42062c4b55ec66204f973715fa65aa52817f1590ef528544ad6de100850c65",
		},
		{
			name: "create key",
			got: func(t *testing.T) string {
				key, err := CreateKey("intent-0001", 0, ProviderKeyCodecV1)
				if err != nil {
					t.Fatalf("build the create key: %v", err)
				}
				return key
			},
			want: "intent-0001:2355823f7909badc61645c53201750901240c6cfec426571c07543454e30ac9a",
		},
		{
			name: "mutation key",
			got: func(t *testing.T) string {
				key, err := MutationKey("intent-0001", OperationUpdate, 3, ProviderKeyCodecV1)
				if err != nil {
					t.Fatalf("build the mutation key: %v", err)
				}
				return key
			},
			want: "intent-0001:84400a262b673217eaf38e01f415365b947e58c4c0d466b4b581d9cc189352b1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got(t); got != tc.want {
				t.Fatalf("%s\n got: %s\nwant: %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestKeysSeparateScopes is the cross-scope check: everything the grammar says
// makes two commitments different has to actually make them different, and the
// two kinds must not collide with each other either.
func TestKeysSeparateScopes(t *testing.T) {
	base := fixtureIntent()
	baseKey := mustIntentKey(t, base, KindEscalation)

	vary := []struct {
		name   string
		change func(*escalationIntent)
	}{
		{"another group", func(i *escalationIntent) { i.AlertGroupID = "ag-0002" }},
		{"another slot kind", func(i *escalationIntent) { i.Slot = Slot{Kind: SlotFirehose} }},
		{"another policy step", func(i *escalationIntent) { i.Slot.Index = 3 }},
		{"another provider", func(i *escalationIntent) { i.Provider = "telegram" }},
		{"another target kind", func(i *escalationIntent) { i.Target.Kind = TargetUser }},
		{"another recipient", func(i *escalationIntent) { i.Target.Ref = "C0002" }},
	}

	seen := map[string]string{baseKey: "base"}
	for _, v := range vary {
		t.Run(v.name, func(t *testing.T) {
			changed := base
			v.change(&changed)
			key := mustIntentKey(t, changed, KindEscalation)
			if key == baseKey {
				t.Fatalf("%s produced the same key", v.name)
			}
			if other, ok := seen[key]; ok {
				t.Fatalf("%s collided with %s", v.name, other)
			}
			seen[key] = v.name
		})
	}

	replay := base
	replay.ClientRequestID = fixtureRequest
	replayKey := mustIntentKey(t, replay, KindEscalationReplay)
	if replayKey == baseKey {
		t.Fatal("a re-admission took the same key as the first admission")
	}

	second := replay
	second.ClientRequestID = "req-0002"
	if mustIntentKey(t, second, KindEscalationReplay) == replayKey {
		t.Fatal("two operator requests took the same key")
	}
}

// TestBatchAndIntentKeysDoNotCollide checks the one collision the readable
// prefix makes easy to miss: both keys start with the same group id, so only
// the digest keeps them apart.
func TestBatchAndIntentKeysDoNotCollide(t *testing.T) {
	intent := mustIntentKey(t, fixtureIntent(), KindEscalation)
	batch, err := BatchIdentity{Kind: KindEscalation, AlertGroupID: fixtureGroup}.BatchKey(GrammarV1)
	if err != nil {
		t.Fatalf("build the batch key: %v", err)
	}
	if intent == batch {
		t.Fatal("the admission and one of its commitments share a key")
	}
}

// TestKeysRefuseWhatTheGrammarCannotSay pins the refusals. Each of them is a
// statement that does not exist in this grammar, and encoding it anyway would
// produce an identity nothing else can reproduce.
func TestKeysRefuseWhatTheGrammarCannotSay(t *testing.T) {
	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "no alert group",
			call: func() error {
				i := fixtureIntent()
				i.AlertGroupID = ""
				_, err := i.key(KindEscalation, GrammarV1)
				return err
			},
		},
		{
			name: "no recipient",
			call: func() error {
				i := fixtureIntent()
				i.Target.Ref = ""
				_, err := i.key(KindEscalation, GrammarV1)
				return err
			},
		},
		{
			name: "a target kind from outside the set",
			call: func() error {
				i := fixtureIntent()
				i.Target.Kind = TargetKind("thread")
				_, err := i.key(KindEscalation, GrammarV1)
				return err
			},
		},
		{
			name: "an index on the firehose slot",
			call: func() error {
				i := fixtureIntent()
				i.Slot = Slot{Kind: SlotFirehose, Index: 1}
				_, err := i.key(KindEscalation, GrammarV1)
				return err
			},
		},
		{
			name: "a first admission carrying a request id",
			call: func() error {
				i := fixtureIntent()
				i.ClientRequestID = fixtureRequest
				_, err := i.key(KindEscalation, GrammarV1)
				return err
			},
		},
		{
			name: "a re-admission without one",
			call: func() error {
				_, err := fixtureIntent().key(KindEscalationReplay, GrammarV1)
				return err
			},
		},
		{
			name: "a request id past the limit",
			call: func() error {
				i := fixtureIntent()
				i.ClientRequestID = strings.Repeat("x", MaxClientRequestID+1)
				_, err := i.key(KindEscalationReplay, GrammarV1)
				return err
			},
		},
		{
			name: "an unknown kind",
			call: func() error {
				_, err := fixtureIntent().key(Kind("escalation_v2"), GrammarV1)
				return err
			},
		},
		{
			name: "an unknown slot kind",
			call: func() error {
				i := fixtureIntent()
				i.Slot = Slot{Kind: SlotKind("stage")}
				_, err := i.key(KindEscalation, GrammarV1)
				return err
			},
		},
		{
			name: "a version this build cannot encode",
			call: func() error {
				_, err := fixtureIntent().key(KindEscalation, GrammarV1+1)
				return err
			},
		},
		{
			name: "an unknown provider key codec",
			call: func() error {
				_, err := CreateKey("intent-0001", 0, ProviderKeyCodecV1+1)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("the grammar accepted a statement it cannot make")
			}
			if !errors.Is(err, ErrContract) {
				t.Fatalf("expected a contract violation, got: %v", err)
			}
		})
	}
}

// TestCandidateBatchKeysAsksInThePlural pins the shape of the lookup rather
// than its length. A repeat of an admission has to find the row the first
// attempt wrote, whatever version wrote it; there is one version today, and
// the caller still has to ask the question as a list.
func TestCandidateBatchKeysAsksInThePlural(t *testing.T) {
	identity := BatchIdentity{Kind: KindEscalation, AlertGroupID: fixtureGroup}

	candidates, err := CandidateBatchKeys(identity)
	if err != nil {
		t.Fatalf("candidate keys: %v", err)
	}
	versions, err := supportedVersions(identity.Kind)
	if err != nil {
		t.Fatalf("supported versions: %v", err)
	}
	if len(candidates) != len(versions) {
		t.Fatalf("got %d candidates for %d supported versions", len(candidates), len(versions))
	}

	current, err := identity.BatchKey(GrammarV1)
	if err != nil {
		t.Fatalf("current key: %v", err)
	}
	if candidates[0] != current {
		t.Fatalf("the newest candidate is not the current key:\n got: %s\nwant: %s",
			candidates[0], current)
	}
}

// TestProviderKeysSeparateEffects checks what the provider is asked to
// deduplicate on. A retry of the same effect has to arrive under the same key,
// a new effect under a new one, and one revision must never be applied twice
// under two different keys.
func TestProviderKeysSeparateEffects(t *testing.T) {
	first, err := CreateKey("intent-0001", 0, ProviderKeyCodecV1)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	same, err := CreateKey("intent-0001", 0, ProviderKeyCodecV1)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if first != same {
		t.Fatal("a retry of one effect asked the provider for a different key")
	}

	next, err := CreateKey("intent-0001", 1, ProviderKeyCodecV1)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if next == first {
		t.Fatal("a new external effect reused the previous key")
	}

	update3, err := MutationKey("intent-0001", OperationUpdate, 3, ProviderKeyCodecV1)
	if err != nil {
		t.Fatalf("mutation key: %v", err)
	}
	update4, err := MutationKey("intent-0001", OperationUpdate, 4, ProviderKeyCodecV1)
	if err != nil {
		t.Fatalf("mutation key: %v", err)
	}
	resolve3, err := MutationKey("intent-0001", OperationResolve, 3, ProviderKeyCodecV1)
	if err != nil {
		t.Fatalf("mutation key: %v", err)
	}
	if update3 == update4 || update3 == resolve3 {
		t.Fatal("two different mutations share a key")
	}
	if update3 == first {
		t.Fatal("a mutation and a creation share a key")
	}
}

// TestKeysStayInsideAnIndexEntry is why the digest is there at all.
//
// A key with a spelled-out recipient list has no upper length, and a b-tree
// entry stops at about 2704 bytes: past it the insert fails, which means the
// commitment cannot be recorded and the delivery it stands for silently does
// not happen. The digest bounds the key at the prefix plus 65 bytes whatever
// goes in.
func TestKeysStayInsideAnIndexEntry(t *testing.T) {
	const btreeLimit = 2704

	huge := fixtureIntent()
	huge.Target.Ref = strings.Repeat("U", 4096)
	huge.ClientRequestID = strings.Repeat("x", MaxClientRequestID)

	key, err := huge.key(KindEscalationReplay, GrammarV1)
	if err != nil {
		t.Fatalf("build the intent key: %v", err)
	}
	if len(key) > btreeLimit {
		t.Fatalf("the key is %d bytes, past the %d an index entry holds", len(key), btreeLimit)
	}

	modest := mustIntentKey(t, fixtureIntent(), KindEscalation)
	if len(key) != len(modest) {
		t.Fatalf("key length moved with the input: %d against %d", len(key), len(modest))
	}
}

// TestKeysAreStableAcrossCalls pins the property a repeat depends on: the same
// statement always spells the same key. A producer retrying an admission after
// a lost reply has to find the row it wrote, not write a second one.
func TestKeysAreStableAcrossCalls(t *testing.T) {
	first := mustIntentKey(t, fixtureIntent(), KindEscalation)
	for i := 0; i < 100; i++ {
		if again := mustIntentKey(t, fixtureIntent(), KindEscalation); again != first {
			t.Fatalf("call %d produced a different key:\n got: %s\nwant: %s", i, again, first)
		}
	}
}
