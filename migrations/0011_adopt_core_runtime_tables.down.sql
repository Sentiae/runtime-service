-- D-178 reverse — drop the core runtime tables adopted from AutoMigrate.
-- No FKs among them (GORM created PKs + indexes only), so order is immaterial.
DROP TABLE IF EXISTS agent_routing_policies;
DROP TABLE IF EXISTS execution_metrics;
DROP TABLE IF EXISTS executions;
DROP TABLE IF EXISTS graph_debug_sessions;
DROP TABLE IF EXISTS graph_definitions;
DROP TABLE IF EXISTS graph_edges;
DROP TABLE IF EXISTS graph_execution_traces;
DROP TABLE IF EXISTS graph_executions;
DROP TABLE IF EXISTS graph_nodes;
DROP TABLE IF EXISTS graph_trace_node_snapshots;
DROP TABLE IF EXISTS hermetic_build_steps;
DROP TABLE IF EXISTS hermetic_builds;
DROP TABLE IF EXISTS image_workloads;
DROP TABLE IF EXISTS micro_vms;
DROP TABLE IF EXISTS node_executions;
DROP TABLE IF EXISTS regression_test_templates;
DROP TABLE IF EXISTS runtime_agents;
DROP TABLE IF EXISTS snapshots;
DROP TABLE IF EXISTS step_artifact_hashes;
DROP TABLE IF EXISTS terminal_sessions;
DROP TABLE IF EXISTS test_runs;
DROP TABLE IF EXISTS vm_instances;
DROP TABLE IF EXISTS vm_usage_records;
