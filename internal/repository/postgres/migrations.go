package postgres

import (
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

// AutoMigrate runs auto-migration for all runtime service models
func AutoMigrate(db *gorm.DB) error {
	models := []interface{}{
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
	}

	if err := db.AutoMigrate(models...); err != nil {
		return err
	}

	return nil
}
