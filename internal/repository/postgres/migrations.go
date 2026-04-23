package postgres

import (
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

// AutoMigrate runs auto-migration for all runtime service models
func AutoMigrate(db *gorm.DB) error {
	models := []any{
		&domain.Execution{},
		&domain.MicroVM{},
		&domain.Snapshot{},
		&domain.ExecutionMetrics{},
		&domain.VMInstance{},
		&domain.GraphDefinition{},
		&domain.GraphNode{},
		&domain.GraphEdge{},
		&domain.GraphExecution{},
		&domain.NodeExecution{},
		&domain.GraphDebugSession{},
		&domain.GraphExecutionTrace{},
		&domain.GraphTraceNodeSnapshot{},
		&domain.TerminalSession{},
		&domain.TestRun{},
		&domain.VMUsageRecord{},

		// 9.4 — customer-hosted Firecracker agent dispatch
		&domain.RuntimeAgent{},
		&domain.AgentRoutingPolicy{},

		// 9.x audit follow-ups: regression tests generated from
		// captured production traces.
		&domain.RegressionTestTemplate{},

		// §9.2 — HermeticBuild row tracks pinned inputs + output
		// digest for reproducible builds.
		&domain.HermeticBuild{},
		&domain.HermeticBuildStep{},
		&domain.StepArtifactHash{},
	}

	if err := db.AutoMigrate(models...); err != nil {
		return err
	}

	return nil
}
