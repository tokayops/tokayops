package builders

import (
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

// WrapStepsInStages creates one stage per step (sequential execution model).
// Stage 0 is "active", all others are "blocked".
// Mutates each step's StageID in place.
func WrapStepsInStages(jobID string, steps []*model.JobStep, now time.Time) []*model.JobStage {
	stages := make([]*model.JobStage, 0, len(steps))
	for i, step := range steps {
		stageID := uuid.New().String()
		step.StageID = stageID
		status := model.JobStageStatusBlocked
		if i == 0 {
			status = model.JobStageStatusActive
		}
		stages = append(stages, &model.JobStage{
			ID:         stageID,
			JobID:      jobID,
			StageIndex: i,
			Status:     status,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	}
	return stages
}
