-- D-178 — golang-migrate becomes the SOLE schema authority (retire AutoMigrate).
-- Closes #runtime-automigrate-vs-golang-migrate: until now the core runtime
-- tables (execution/graph-interpreter/test/hermetic-build/vm/image-workload)
-- were created at boot by GORM AutoMigrate (internal/repository/postgres/
-- migrations.go), while golang-migrate owned only the fleet control plane
-- (0001..0010). This migration adopts EXACTLY the AutoMigrate delta so a fresh
-- deploy of 0001..0011 reproduces the full schema with zero AutoMigrate.
-- The live DB is already at v10 with these tables present (created by the old
-- AutoMigrate); this migration is a no-op there (IF NOT EXISTS) and the sole
-- creator on any clean database.
--
-- Verbatim from GORM AutoMigrate output (pg_dump of the model set), so the
-- schema is byte-identical to what the models expect. No FKs among these
-- tables (GORM created PKs + indexes only).
CREATE TABLE IF NOT EXISTS agent_routing_policies (
    organization_id uuid NOT NULL,
    preferred_agent_id uuid,
    fallback_to_local boolean DEFAULT true,
    label_selectors jsonb,
    updated_at timestamp with time zone,
    PRIMARY KEY (organization_id)
);

CREATE TABLE IF NOT EXISTS execution_metrics (
    id uuid NOT NULL,
    execution_id uuid NOT NULL,
    vm_id uuid NOT NULL,
    cpu_time_ms bigint DEFAULT 0 NOT NULL,
    memory_peak_mb numeric DEFAULT 0 NOT NULL,
    memory_avg_mb numeric DEFAULT 0 NOT NULL,
    io_read_bytes bigint DEFAULT 0 NOT NULL,
    io_write_bytes bigint DEFAULT 0 NOT NULL,
    net_bytes_in bigint DEFAULT 0 NOT NULL,
    net_bytes_out bigint DEFAULT 0 NOT NULL,
    boot_time_ms bigint DEFAULT 0 NOT NULL,
    compile_time_ms bigint,
    exec_time_ms bigint DEFAULT 0 NOT NULL,
    total_time_ms bigint DEFAULT 0 NOT NULL,
    collected_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS executions (
    id uuid NOT NULL,
    organization_id uuid NOT NULL,
    requested_by uuid NOT NULL,
    node_id uuid,
    workflow_id uuid,
    language character varying(20) NOT NULL,
    code text NOT NULL,
    stdin text,
    args jsonb,
    env_vars jsonb,
    status character varying(20) DEFAULT 'pending'::character varying NOT NULL,
    exit_code bigint,
    stdout text,
    stderr text,
    error text,
    vm_id uuid,
    resource_vcpu bigint DEFAULT 1 NOT NULL,
    resource_memory_mb bigint DEFAULT 128 NOT NULL,
    resource_timeout_sec bigint DEFAULT 30 NOT NULL,
    resource_network character varying(20) DEFAULT 'isolated'::character varying NOT NULL,
    resource_disk_mb bigint DEFAULT 256 NOT NULL,
    resource_disk_bandwidth_mbps bigint DEFAULT 0 NOT NULL,
    resource_disk_iops bigint DEFAULT 0 NOT NULL,
    resource_network_bandwidth_mbps bigint DEFAULT 0 NOT NULL,
    resource_network_pps bigint DEFAULT 0 NOT NULL,
    started_at timestamp with time zone,
    completed_at timestamp with time zone,
    duration_ms bigint,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS graph_debug_sessions (
    id uuid NOT NULL,
    graph_execution_id uuid NOT NULL,
    graph_id uuid NOT NULL,
    organization_id uuid NOT NULL,
    user_id uuid NOT NULL,
    mode character varying(50) DEFAULT 'step_through'::character varying NOT NULL,
    status character varying(50) DEFAULT 'created'::character varying NOT NULL,
    current_node_id uuid,
    breakpoints jsonb,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS graph_definitions (
    id uuid NOT NULL,
    organization_id uuid NOT NULL,
    canvas_id uuid,
    workflow_id uuid,
    name character varying(255) NOT NULL,
    description text,
    version bigint DEFAULT 1 NOT NULL,
    status character varying(20) DEFAULT 'draft'::character varying NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS graph_edges (
    id uuid NOT NULL,
    graph_id uuid NOT NULL,
    source_node_id uuid NOT NULL,
    target_node_id uuid NOT NULL,
    source_port character varying(100) DEFAULT 'output'::character varying NOT NULL,
    target_port character varying(100) DEFAULT 'input'::character varying NOT NULL,
    condition_expr text,
    created_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS graph_execution_traces (
    id uuid NOT NULL,
    graph_execution_id uuid NOT NULL,
    graph_id uuid NOT NULL,
    organization_id uuid NOT NULL,
    status character varying(50) DEFAULT 'recording'::character varying NOT NULL,
    total_nodes bigint DEFAULT 0 NOT NULL,
    total_duration_ms numeric,
    trigger_data jsonb,
    created_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS graph_executions (
    id uuid NOT NULL,
    graph_id uuid NOT NULL,
    organization_id uuid NOT NULL,
    requested_by uuid NOT NULL,
    status character varying(20) DEFAULT 'pending'::character varying NOT NULL,
    input jsonb,
    output jsonb,
    error text,
    total_nodes bigint DEFAULT 0 NOT NULL,
    completed_nodes bigint DEFAULT 0 NOT NULL,
    debug_mode boolean DEFAULT false NOT NULL,
    started_at timestamp with time zone,
    completed_at timestamp with time zone,
    duration_ms bigint,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS graph_nodes (
    id uuid NOT NULL,
    graph_id uuid NOT NULL,
    node_type character varying(50) NOT NULL,
    name character varying(255) NOT NULL,
    config jsonb,
    language character varying(20),
    code text,
    resource_vcpu bigint DEFAULT 1 NOT NULL,
    resource_memory_mb bigint DEFAULT 128 NOT NULL,
    resource_timeout_sec bigint DEFAULT 30 NOT NULL,
    resource_network character varying(20) DEFAULT 'isolated'::character varying NOT NULL,
    resource_disk_mb bigint DEFAULT 256 NOT NULL,
    resource_disk_bandwidth_mbps bigint DEFAULT 0 NOT NULL,
    resource_disk_iops bigint DEFAULT 0 NOT NULL,
    resource_network_bandwidth_mbps bigint DEFAULT 0 NOT NULL,
    resource_network_pps bigint DEFAULT 0 NOT NULL,
    "position" jsonb,
    sort_order bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS graph_trace_node_snapshots (
    id uuid NOT NULL,
    trace_id uuid NOT NULL,
    graph_node_id uuid NOT NULL,
    node_name character varying(255),
    node_type character varying(100),
    sequence_number bigint NOT NULL,
    input jsonb,
    output jsonb,
    config jsonb,
    status character varying(50) NOT NULL,
    error text,
    duration_ms numeric,
    input_size_bytes bigint,
    output_size_bytes bigint,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS hermetic_build_steps (
    id uuid NOT NULL,
    build_id uuid NOT NULL,
    index bigint NOT NULL,
    name character varying(200) NOT NULL,
    language character varying(40) NOT NULL,
    command text NOT NULL,
    input_artifact character varying(500),
    output_artifact character varying(500),
    env_json jsonb,
    timeout_sec bigint DEFAULT 0 NOT NULL,
    exit_code bigint,
    stdout text,
    stderr text,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS hermetic_builds (
    id uuid NOT NULL,
    organization_id uuid NOT NULL,
    pipeline_run_id uuid NOT NULL,
    service_id uuid,
    spec_id uuid,
    session_id uuid,
    feature_ids jsonb,
    input_digest character varying(128) NOT NULL,
    output_digest character varying(128),
    artifact_ref character varying(500),
    reproducible boolean DEFAULT false NOT NULL,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    created_by uuid NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS image_workloads (
    id uuid NOT NULL,
    component_id character varying(255),
    env character varying(64),
    owner_org character varying(64),
    idempotency_key character varying(255),
    job_command jsonb,
    egress_allow jsonb,
    image_repository character varying(512),
    image_digest character varying(255),
    class character varying(20) NOT NULL,
    state character varying(20) DEFAULT 'booting'::character varying NOT NULL,
    guest_ip character varying(45),
    host_port bigint DEFAULT 0 NOT NULL,
    port bigint DEFAULT 0 NOT NULL,
    rootfs_path character varying(512),
    socket_path character varying(512),
    tap_name character varying(32),
    net_index bigint DEFAULT 0 NOT NULL,
    p_id bigint,
    exit_code bigint,
    stdout_tail text,
    stderr_tail text,
    url character varying(512),
    message text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS micro_vms (
    id uuid NOT NULL,
    status character varying(20) DEFAULT 'creating'::character varying NOT NULL,
    v_cpu bigint DEFAULT 1 NOT NULL,
    memory_mb bigint DEFAULT 128 NOT NULL,
    kernel_path character varying(500) NOT NULL,
    rootfs_path character varying(500) NOT NULL,
    socket_path character varying(500),
    p_id bigint,
    ip_address character varying(45),
    network_mode character varying(20) DEFAULT 'isolated'::character varying NOT NULL,
    language character varying(20) NOT NULL,
    boot_time_ms bigint,
    execution_id uuid,
    created_at timestamp with time zone NOT NULL,
    terminated_at timestamp with time zone,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS node_executions (
    id uuid NOT NULL,
    graph_execution_id uuid NOT NULL,
    graph_node_id uuid NOT NULL,
    node_type character varying(50) NOT NULL,
    node_name character varying(255),
    sequence_number bigint NOT NULL,
    status character varying(20) DEFAULT 'pending'::character varying NOT NULL,
    input jsonb,
    output jsonb,
    error text,
    execution_id uuid,
    started_at timestamp with time zone,
    completed_at timestamp with time zone,
    duration_ms bigint,
    cached boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS regression_test_templates (
    id uuid NOT NULL,
    organization_id uuid NOT NULL,
    trace_id character varying(128) NOT NULL,
    service_id character varying(128) NOT NULL,
    language character varying(20) NOT NULL,
    framework character varying(40) NOT NULL,
    generated_code text NOT NULL,
    trace_summary text,
    notes text,
    test_run_id uuid,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS runtime_agents (
    id uuid NOT NULL,
    organization_id uuid NOT NULL,
    name character varying(200) NOT NULL,
    endpoint character varying(500) NOT NULL,
    token_hash character varying(200) NOT NULL,
    fingerprint character varying(100),
    status character varying(20) DEFAULT 'pending'::character varying NOT NULL,
    capabilities jsonb,
    labels jsonb,
    last_seen_at timestamp with time zone,
    last_error text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS snapshots (
    id uuid NOT NULL,
    vm_id uuid NOT NULL,
    execution_id uuid,
    language character varying(20) NOT NULL,
    memory_file_path character varying(500) NOT NULL,
    state_file_path character varying(500) NOT NULL,
    memory_object_key character varying(500),
    state_object_key character varying(500),
    size_bytes bigint NOT NULL,
    v_cpu bigint NOT NULL,
    memory_mb bigint NOT NULL,
    description text,
    is_base_image boolean DEFAULT false NOT NULL,
    kind character varying(20) DEFAULT 'manual'::character varying NOT NULL,
    restore_time_ms bigint,
    created_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS step_artifact_hashes (
    id uuid NOT NULL,
    build_id uuid NOT NULL,
    step_index bigint NOT NULL,
    digest character varying(128) NOT NULL,
    artifact_ref character varying(500),
    created_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS terminal_sessions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    vm_id uuid NOT NULL,
    language character varying(20) NOT NULL,
    repo_id uuid,
    status character varying(20) DEFAULT 'creating'::character varying NOT NULL,
    ip_address character varying(45),
    created_at timestamp with time zone NOT NULL,
    closed_at timestamp with time zone,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS test_runs (
    id uuid NOT NULL,
    organization_id uuid NOT NULL,
    execution_id uuid NOT NULL,
    test_node_id uuid NOT NULL,
    code_node_id uuid,
    canvas_id uuid,
    service_id uuid,
    spec_id uuid,
    session_id uuid,
    feature_ids jsonb,
    language character varying(20) NOT NULL,
    test_type character varying(20) DEFAULT 'unit'::character varying NOT NULL,
    status character varying(20) DEFAULT 'running'::character varying NOT NULL,
    passed bigint DEFAULT 0,
    failed bigint DEFAULT 0,
    skipped bigint DEFAULT 0,
    total bigint DEFAULT 0,
    coverage_pc numeric,
    duration_ms bigint,
    error_message text,
    retry_count bigint DEFAULT 0,
    max_retries bigint DEFAULT 2,
    was_retried boolean DEFAULT false,
    framework character varying(40),
    timeout_ms bigint DEFAULT 0,
    flakiness_score numeric DEFAULT 0,
    critical boolean DEFAULT false,
    quarantined boolean DEFAULT false,
    quarantined_at timestamp with time zone,
    result_json jsonb,
    db_mode character varying(30) DEFAULT 'none'::character varying,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS vm_instances (
    id uuid NOT NULL,
    execution_id uuid,
    host_id character varying(255) NOT NULL,
    state character varying(20) DEFAULT 'pending'::character varying NOT NULL,
    desired_state character varying(20) DEFAULT 'running'::character varying NOT NULL,
    language character varying(20) NOT NULL,
    base_image character varying(500),
    v_cpu bigint DEFAULT 1 NOT NULL,
    memory_mb bigint DEFAULT 128 NOT NULL,
    disk_mb bigint DEFAULT 256 NOT NULL,
    ip_address character varying(45),
    socket_path character varying(500),
    p_id bigint,
    error_message text,
    checkpoint_interval_seconds bigint DEFAULT 0 NOT NULL,
    last_checkpoint_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    terminated_at timestamp with time zone,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS vm_usage_records (
    id uuid NOT NULL,
    organization_id uuid NOT NULL,
    execution_id uuid NOT NULL,
    language character varying(20) NOT NULL,
    v_cpu bigint DEFAULT 1 NOT NULL,
    memory_mb bigint DEFAULT 128 NOT NULL,
    execution_time_ms bigint DEFAULT 0 NOT NULL,
    boot_time_ms bigint DEFAULT 0 NOT NULL,
    reused boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone NOT NULL,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_execution_metrics_execution_id ON execution_metrics USING btree (execution_id);

CREATE INDEX IF NOT EXISTS idx_execution_metrics_vm_id ON execution_metrics USING btree (vm_id);

CREATE INDEX IF NOT EXISTS idx_executions_node_id ON executions USING btree (node_id);

CREATE INDEX IF NOT EXISTS idx_executions_organization_id ON executions USING btree (organization_id);

CREATE INDEX IF NOT EXISTS idx_executions_requested_by ON executions USING btree (requested_by);

CREATE INDEX IF NOT EXISTS idx_executions_status ON executions USING btree (status);

CREATE INDEX IF NOT EXISTS idx_executions_vm_id ON executions USING btree (vm_id);

CREATE INDEX IF NOT EXISTS idx_executions_workflow_id ON executions USING btree (workflow_id);

CREATE INDEX IF NOT EXISTS idx_graph_debug_sessions_graph_execution_id ON graph_debug_sessions USING btree (graph_execution_id);

CREATE INDEX IF NOT EXISTS idx_graph_debug_sessions_graph_id ON graph_debug_sessions USING btree (graph_id);

CREATE INDEX IF NOT EXISTS idx_graph_debug_sessions_organization_id ON graph_debug_sessions USING btree (organization_id);

CREATE INDEX IF NOT EXISTS idx_graph_definitions_canvas_id ON graph_definitions USING btree (canvas_id);

CREATE INDEX IF NOT EXISTS idx_graph_definitions_organization_id ON graph_definitions USING btree (organization_id);

CREATE INDEX IF NOT EXISTS idx_graph_definitions_status ON graph_definitions USING btree (status);

CREATE INDEX IF NOT EXISTS idx_graph_definitions_workflow_id ON graph_definitions USING btree (workflow_id);

CREATE INDEX IF NOT EXISTS idx_graph_edges_graph_id ON graph_edges USING btree (graph_id);

CREATE INDEX IF NOT EXISTS idx_graph_edges_source_node_id ON graph_edges USING btree (source_node_id);

CREATE INDEX IF NOT EXISTS idx_graph_edges_target_node_id ON graph_edges USING btree (target_node_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_graph_execution_traces_graph_execution_id ON graph_execution_traces USING btree (graph_execution_id);

CREATE INDEX IF NOT EXISTS idx_graph_execution_traces_graph_id ON graph_execution_traces USING btree (graph_id);

CREATE INDEX IF NOT EXISTS idx_graph_execution_traces_organization_id ON graph_execution_traces USING btree (organization_id);

CREATE INDEX IF NOT EXISTS idx_graph_executions_graph_id ON graph_executions USING btree (graph_id);

CREATE INDEX IF NOT EXISTS idx_graph_executions_organization_id ON graph_executions USING btree (organization_id);

CREATE INDEX IF NOT EXISTS idx_graph_executions_status ON graph_executions USING btree (status);

CREATE INDEX IF NOT EXISTS idx_graph_nodes_graph_id ON graph_nodes USING btree (graph_id);

CREATE INDEX IF NOT EXISTS idx_graph_trace_node_snapshots_graph_node_id ON graph_trace_node_snapshots USING btree (graph_node_id);

CREATE INDEX IF NOT EXISTS idx_graph_trace_node_snapshots_trace_id ON graph_trace_node_snapshots USING btree (trace_id);

CREATE INDEX IF NOT EXISTS idx_hermetic_build_steps_build_id ON hermetic_build_steps USING btree (build_id);

CREATE INDEX IF NOT EXISTS idx_hermetic_builds_input_digest ON hermetic_builds USING btree (input_digest);

CREATE INDEX IF NOT EXISTS idx_hermetic_builds_organization_id ON hermetic_builds USING btree (organization_id);

CREATE INDEX IF NOT EXISTS idx_hermetic_builds_output_digest ON hermetic_builds USING btree (output_digest);

CREATE INDEX IF NOT EXISTS idx_hermetic_builds_pipeline_run_id ON hermetic_builds USING btree (pipeline_run_id);

CREATE INDEX IF NOT EXISTS idx_hermetic_builds_service_id ON hermetic_builds USING btree (service_id);

CREATE INDEX IF NOT EXISTS idx_hermetic_builds_session_id ON hermetic_builds USING btree (session_id);

CREATE INDEX IF NOT EXISTS idx_hermetic_builds_spec_id ON hermetic_builds USING btree (spec_id);

CREATE INDEX IF NOT EXISTS idx_image_workloads_class ON image_workloads USING btree (class);

CREATE INDEX IF NOT EXISTS idx_image_workloads_component_id ON image_workloads USING btree (component_id);

CREATE INDEX IF NOT EXISTS idx_image_workloads_image_digest ON image_workloads USING btree (image_digest);

CREATE UNIQUE INDEX IF NOT EXISTS idx_image_workloads_owner_idem ON image_workloads USING btree (owner_org, idempotency_key);

CREATE INDEX IF NOT EXISTS idx_image_workloads_owner_org ON image_workloads USING btree (owner_org);

CREATE INDEX IF NOT EXISTS idx_image_workloads_state ON image_workloads USING btree (state);

CREATE INDEX IF NOT EXISTS idx_micro_vms_execution_id ON micro_vms USING btree (execution_id);

CREATE INDEX IF NOT EXISTS idx_micro_vms_status ON micro_vms USING btree (status);

CREATE INDEX IF NOT EXISTS idx_node_executions_execution_id ON node_executions USING btree (execution_id);

CREATE INDEX IF NOT EXISTS idx_node_executions_graph_execution_id ON node_executions USING btree (graph_execution_id);

CREATE INDEX IF NOT EXISTS idx_node_executions_graph_node_id ON node_executions USING btree (graph_node_id);

CREATE INDEX IF NOT EXISTS idx_regression_test_templates_organization_id ON regression_test_templates USING btree (organization_id);

CREATE INDEX IF NOT EXISTS idx_regression_test_templates_service_id ON regression_test_templates USING btree (service_id);

CREATE INDEX IF NOT EXISTS idx_regression_test_templates_test_run_id ON regression_test_templates USING btree (test_run_id);

CREATE INDEX IF NOT EXISTS idx_regression_test_templates_trace_id ON regression_test_templates USING btree (trace_id);

CREATE INDEX IF NOT EXISTS idx_runtime_agents_organization_id ON runtime_agents USING btree (organization_id);

CREATE INDEX IF NOT EXISTS idx_snap_vm_kind ON snapshots USING btree (kind, created_at);

CREATE INDEX IF NOT EXISTS idx_snapshots_execution_id ON snapshots USING btree (execution_id);

CREATE INDEX IF NOT EXISTS idx_snapshots_is_base_image ON snapshots USING btree (is_base_image);

CREATE INDEX IF NOT EXISTS idx_snapshots_language ON snapshots USING btree (language);

CREATE INDEX IF NOT EXISTS idx_snapshots_vm_id ON snapshots USING btree (vm_id);

CREATE INDEX IF NOT EXISTS idx_step_artifact_hashes_build_id ON step_artifact_hashes USING btree (build_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_step_hash_build_step ON step_artifact_hashes USING btree (build_id, step_index);

CREATE INDEX IF NOT EXISTS idx_terminal_sessions_status ON terminal_sessions USING btree (status);

CREATE INDEX IF NOT EXISTS idx_terminal_sessions_user_id ON terminal_sessions USING btree (user_id);

CREATE INDEX IF NOT EXISTS idx_test_runs_canvas_id ON test_runs USING btree (canvas_id);

CREATE INDEX IF NOT EXISTS idx_test_runs_code_node_id ON test_runs USING btree (code_node_id);

CREATE INDEX IF NOT EXISTS idx_test_runs_critical ON test_runs USING btree (critical);

CREATE INDEX IF NOT EXISTS idx_test_runs_execution_id ON test_runs USING btree (execution_id);

CREATE INDEX IF NOT EXISTS idx_test_runs_node ON test_runs USING btree (test_node_id);

CREATE INDEX IF NOT EXISTS idx_test_runs_organization_id ON test_runs USING btree (organization_id);

CREATE INDEX IF NOT EXISTS idx_test_runs_quarantined ON test_runs USING btree (quarantined);

CREATE INDEX IF NOT EXISTS idx_test_runs_service_id ON test_runs USING btree (service_id);

CREATE INDEX IF NOT EXISTS idx_test_runs_session_id ON test_runs USING btree (session_id);

CREATE INDEX IF NOT EXISTS idx_test_runs_spec_id ON test_runs USING btree (spec_id);

CREATE INDEX IF NOT EXISTS idx_test_runs_status ON test_runs USING btree (status);

CREATE INDEX IF NOT EXISTS idx_test_runs_test_type ON test_runs USING btree (test_type);

CREATE INDEX IF NOT EXISTS idx_vm_instances_execution_id ON vm_instances USING btree (execution_id);

CREATE INDEX IF NOT EXISTS idx_vm_instances_host_id ON vm_instances USING btree (host_id);

CREATE INDEX IF NOT EXISTS idx_vm_instances_state ON vm_instances USING btree (state);

CREATE INDEX IF NOT EXISTS idx_vm_usage_org ON vm_usage_records USING btree (organization_id, created_at);

CREATE INDEX IF NOT EXISTS idx_vm_usage_records_execution_id ON vm_usage_records USING btree (execution_id);
