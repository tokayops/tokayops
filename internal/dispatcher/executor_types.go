package dispatcher

import (
	"context"

	"github.com/tokayops/tokayops/internal/model"
)

// StepExecutor defines the interface for executing a job step.
type StepExecutor interface {
	Execute(ctx context.Context, job *model.Job, step *model.JobStep) (string, error)
}
