package providers_test

// Both channels are what the worker expects, and the compiler is the one that
// says so: a channel that stopped implementing the domain's interface would
// otherwise only be found at wiring time, in main.
import (
	"github.com/tokayops/tokayops/internal/outbound"
	slackprovider "github.com/tokayops/tokayops/internal/outbound/providers/slack"
	telegramprovider "github.com/tokayops/tokayops/internal/outbound/providers/telegram"
)

var (
	_ outbound.Channel = (*slackprovider.Handler)(nil)
	_ outbound.Channel = (*telegramprovider.Handler)(nil)
)
