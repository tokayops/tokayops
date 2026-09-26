package twilio

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/tokayops/tokayops/internal/model"
)

// Integration is one enabled Twilio integration, its config read.
type Integration struct {
	ID     string
	Name   string
	Config model.TwilioConfig
}

// Decode reads the Twilio integrations out of stored rows. A row whose config
// does not read is an error naming it rather than a row skipped: a provider
// that silently drops out changes who can be called.
func Decode(rows []*model.Integration) ([]Integration, error) {
	out := make([]Integration, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Type != model.IntegrationTypeTwilio || !row.Enabled {
			continue
		}
		var cfg model.TwilioConfig
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			return nil, fmt.Errorf("read the config of twilio integration %s: %w", row.ID, err)
		}
		out = append(out, Integration{ID: row.ID, Name: row.Name, Config: cfg})
	}
	return out, nil
}

// Covering returns the integrations that may call number, in the order they
// are tried: priority, then id so the order does not depend on the database.
func Covering(all []Integration, number string) []Integration {
	var out []Integration
	for _, in := range all {
		if in.Config.Covers(number) {
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Config.Priority != out[j].Config.Priority {
			return out[i].Config.Priority < out[j].Config.Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Pick chooses the integration to call number through: the pinned one if it
// covers the number, otherwise the first that does. A pin is a preference and
// not a ban - the person wants to be reached, not reached by one carrier.
func Pick(all []Integration, number, pinned string) (Integration, bool) {
	covering := Covering(all, number)
	if len(covering) == 0 {
		return Integration{}, false
	}
	for _, in := range covering {
		if in.ID == pinned {
			return in, true
		}
	}
	return covering[0], true
}

// AccountLimits is the rate and concurrency an account may be used at: the
// lowest each integration on that account states. The provider applies its
// limits per account, and two integrations that disagree are both obeyed by
// taking the smaller - no form has to be serialised against another for it.
func AccountLimits(all []Integration, accountSID string) (cps, maxConcurrent int) {
	for _, in := range all {
		if in.Config.AccountSID != accountSID {
			continue
		}
		if cps == 0 || in.Config.CPS < cps {
			cps = in.Config.CPS
		}
		if maxConcurrent == 0 || in.Config.MaxConcurrent < maxConcurrent {
			maxConcurrent = in.Config.MaxConcurrent
		}
	}
	return cps, maxConcurrent
}
