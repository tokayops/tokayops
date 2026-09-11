package keys

import (
	"bytes"
	"encoding/json"
)

// Reading the snapshots version 1 wrote.
//
// Version 1 had one form, one digest and no history. Its rows live in two
// places: the group's own state, which the start-up of the first version 2
// build rebuilds into version 2 and does not leave behind, and the admission
// state of every batch admitted before that, which is frozen and stays as it
// was - a one-shot message retried after the upgrade still renders it. Those
// are the two readers, and TestSnapshotV1HasTwoReaders names them.

// renderSnapshotV1Protocol is the separator version 1 hashed under.
const renderSnapshotV1Protocol = "render_snapshot/v1"

// snapshotInputV1 is the stored shape of version 1, field for field. It is
// its own struct rather than the current one with three fields ignored,
// because a version 1 row carrying a history is not a version 1 row, and the
// reader has to be able to say so.
type snapshotInputV1 struct {
	AlertGroupID    string          `json:"alert_group_id"`
	Revision        int64           `json:"revision"`
	Status          GroupStatus     `json:"status"`
	Title           string          `json:"title"`
	Severity        string          `json:"severity"`
	TeamLabel       *string         `json:"team_label,omitempty"`
	TeamOnboarded   bool            `json:"team_onboarded"`
	GroupURL        *string         `json:"group_url,omitempty"`
	ExternalURL     *string         `json:"external_url,omitempty"`
	DisplayTimezone string          `json:"display_timezone"`
	AcknowledgedBy  *string         `json:"acknowledged_by,omitempty"`
	ResolvedBy      *string         `json:"resolved_by,omitempty"`
	Alerts          []AlertSnapshot `json:"alerts"`
	TeamSetupURL    *string         `json:"team_setup_url,omitempty"`
}

// DecodeRenderSnapshotV1 reads a version 1 row, canonicalises it by version
// 1's rules and computes version 1's digest - the one its commitments were
// keyed against, byte for byte.
//
// What comes back renders (a direct message drawn from an old admission shows
// what was admitted) and rebuilds (the start-up takes its content, adds the
// history and the buttons, and stores a version 2 snapshot); it does not
// write. Version 1 did not cut names or titles, and this reader does not
// either: a stored row has to digest to what its digest column says.
func DecodeRenderSnapshotV1(raw []byte) (RenderSnapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var stored snapshotInputV1
	if err := decoder.Decode(&stored); err != nil {
		return RenderSnapshot{}, contractf("a version 1 snapshot cannot be read: %v", err)
	}

	in := SnapshotInput{
		AlertGroupID: stored.AlertGroupID, Revision: stored.Revision,
		Status: stored.Status, Title: stored.Title, Severity: stored.Severity,
		TeamLabel: stored.TeamLabel, TeamOnboarded: stored.TeamOnboarded,
		GroupURL: stored.GroupURL, ExternalURL: stored.ExternalURL,
		DisplayTimezone: stored.DisplayTimezone,
		AcknowledgedBy:  stored.AcknowledgedBy, ResolvedBy: stored.ResolvedBy,
		Alerts: stored.Alerts, TeamSetupURL: stored.TeamSetupURL,
	}
	if err := checkIdentity(in); err != nil {
		return RenderSnapshot{}, err
	}

	out := in.clone()
	if err := canonicalAlerts(out.Alerts, false); err != nil {
		return RenderSnapshot{}, err
	}

	fields, err := encodeFields(out)
	if err != nil {
		return RenderSnapshot{}, err
	}
	return RenderSnapshot{
		content: out,
		schema:  RenderSnapshotSchemaV1,
		digest:  fields.digest(renderSnapshotV1Protocol, v1Tag),
	}, nil
}

// v1Tag is what version 1 hashed: tags 1 to 13 and 15, in that order.
func v1Tag(tag uint32) bool { return tag <= 15 }
