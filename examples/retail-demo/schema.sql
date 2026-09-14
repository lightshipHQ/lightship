CREATE DATABASE IF NOT EXISTS lightship_demo;

CREATE TABLE IF NOT EXISTS lightship_demo.agent_traces (
    Timestamp DateTime64(9, 'UTC'),
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    TenantId LowCardinality(String),
    AgentName LowCardinality(String),
    Environment LowCardinality(String),
    SessionId String,
    ScenarioId String,
    DatasetVersion LowCardinality(String),
    RunnerVersion String,
    PromptVersion String,
    PolicyVersion String,
    Input String,
    Output String,
    ToolName LowCardinality(String),
    ToolArguments String,
    ToolResult String,
    StatusCode LowCardinality(String),
    DurationMs UInt64,
    InputTokens UInt64,
    OutputTokens UInt64,
    ResponseId String
) ENGINE = MergeTree
PARTITION BY DatasetVersion
ORDER BY (TenantId, Timestamp, TraceId, SpanId);

-- Evaluator labels are intentionally outside LightShip's trace binding and reader grant.
CREATE TABLE IF NOT EXISTS lightship_demo.evaluations (
    DatasetVersion LowCardinality(String),
    SessionId String,
    ScenarioId String,
    TraceId String,
    ExpectedFamily LowCardinality(String),
    ObservedFamily LowCardinality(String),
    MatchesExpected Bool,
    ClaimedRefundSuccess Bool,
    RefundStatuses Array(String),
    SettlementTimingExplained Bool,
    PolicyVariant LowCardinality(String)
) ENGINE = MergeTree
PARTITION BY DatasetVersion
ORDER BY (ScenarioId, TraceId);

CREATE TABLE IF NOT EXISTS lightship_demo.dataset_imports (
    DatasetVersion String,
    ContentHash String,
    ImportedAt DateTime64(3, 'UTC'),
    TraceCount UInt32,
    SpanCount UInt32,
    Manifest String
) ENGINE = ReplacingMergeTree(ImportedAt)
ORDER BY DatasetVersion;
