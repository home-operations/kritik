// Mirrors internal/webapi/types.go field for field; the Go side pins the
// JSON names in internal/webapi/testdata/*.golden.json. Timestamps are
// RFC 3339 strings; null means "none".

export type TenantRole = 'admin' | 'member';
export type TenantManagedBy = 'file' | 'dashboard';

export interface TenantMembership {
  slug: string;
  role: TenantRole;
  managedBy: TenantManagedBy;
}

export interface Account {
  id: string;
  displayName: string;
  email: string;
  avatarUrl: string;
}

export interface Me {
  account: Account;
  operator: boolean;
  tenants: TenantMembership[];
}

export type SignInProviderType = 'oidc' | 'github' | 'forgejo';

export interface SignInProvider {
  name: string;
  type: SignInProviderType;
  displayName: string;
}

// A keyset-paginated list: pass nextCursor back as ?cursor= for the next
// page; it is null on the last one.
export interface Page<T> {
  items: T[];
  nextCursor: string | null;
}

export type ErrorCode = 'not_found' | 'bad_request' | 'invalid_cursor' | 'ambiguous' | 'internal';

export interface ErrorBody {
  code: ErrorCode | string;
  message: string;
  details?: unknown;
}

export type ReviewStatus =
  | 'running'
  | 'prepared'
  | 'completed'
  | 'superseded'
  | 'skipped'
  | 'capped'
  | 'failed'
  | 'canceled';
export type ReviewMode = 'single' | 'agentic';
export type ReviewScope = 'full' | 'incremental';
export type SkipReason = '' | 'disabled' | 'filtered' | 'only_skipped_paths';
export type Severity = 'blocking' | 'important' | 'nit';
export type IndexRunStatus = 'running' | 'completed' | 'failed' | 'superseded';
export type FollowupStatus = 'answered' | 'limited' | 'ignored' | 'failed';
export type Forge = 'github' | 'gitlab' | 'forgejo';
export type CredentialKind = 'app' | 'token';
export type UsageGroup = 'day' | 'model' | 'repo' | 'role';
export type JobState =
  | 'available'
  | 'scheduled'
  | 'running'
  | 'retryable'
  | 'pending'
  | 'completed'
  | 'cancelled'
  | 'discarded';
export type EventKind = 'review' | 'runner_run' | 'index_run' | 'followup' | 'model_call';
export type TranscriptKind = 'agent_step' | 'review' | 'fallback' | 'followup';
export type MessageRole = 'user' | 'assistant';

export interface MonthUsage {
  tokens: number;
  costUsd: number;
  tokensPerMonth: number;
  reviewsToday: number;
  reviewsPerDay: number;
}

export interface TenantSummary {
  slug: string;
  managedBy: TenantManagedBy;
  role: TenantRole;
  installations: number;
  repositories: number;
  reviews7d: number;
  usage: MonthUsage;
}

// live is false for a dashboard tenant stored but not in the running
// configuration (it does not validate, or has not been merged yet).
export interface OperatorTenant extends TenantSummary {
  live: boolean;
  revision: number;
}

export interface CredentialsSet {
  clientId: boolean;
  privateKey: boolean;
  token: boolean;
  gitToken: boolean;
  webhookSecret: boolean;
}

export interface Installation {
  name: string;
  forge: Forge;
  host: string;
  account: string;
  credentialKind: CredentialKind;
  credentials: CredentialsSet;
  hookPath: string;
}

export interface Models {
  review: string;
  fallback: string;
}

export interface Limits {
  concurrency: number;
  reviewsPerDay: number;
  tokensPerMonth: number;
}

export interface TenantDetail {
  slug: string;
  managedBy: TenantManagedBy;
  role: TenantRole;
  installations: Installation[];
  models: Models;
  limits: Limits;
  filter: string;
  usage: MonthUsage;
}

export interface IndexState {
  activeCommit: string;
  activeAt: string | null;
  lastRunStatus: IndexRunStatus | '';
  lastRunAt: string | null;
}

export interface ReviewRef {
  id: string;
  status: ReviewStatus;
  createdAt: string;
}

export interface Repository {
  id: string;
  fullName: string;
  installation: string;
  enabled: boolean;
  managedBy: 'file' | 'dashboard' | 'forge';
  defaultBranch: string;
  index: IndexState;
  lastReview: ReviewRef | null;
}

export interface AgentLimits {
  maxSteps: number;
  maxToolOutputBytes: number;
  maxTokens: number;
  timeoutSeconds: number;
  commands: string[];
  commandTimeoutSeconds: number;
}

export interface ReviewBlock {
  instructions: string[];
  requireSuggestedFix: boolean;
}

export interface RepoSettings {
  enabled: boolean;
  mode: ReviewMode;
  models: Models;
  filter: string;
  forks: boolean;
  ignore: string[];
  settleSeconds: number;
  maxDeltaFiles: number;
  review: ReviewBlock;
  agent: AgentLimits;
  limits: Limits;
}

export interface IndexRun {
  id: string;
  repository: string;
  commitSha: string;
  baseSha: string;
  embedModel: string;
  mode: 'full' | 'incremental';
  status: IndexRunStatus;
  trigger: string;
  chunkCount: number;
  error: string;
  createdAt: string;
  finishedAt: string | null;
}

export interface RepoDetail extends Repository {
  settings: RepoSettings;
  indexRuns: IndexRun[];
}

export interface Label {
  name: string;
  color: string;
}

export interface SeverityCounts {
  blocking: number;
  important: number;
  nit: number;
}

export interface ReviewBrief {
  id: string;
  status: ReviewStatus;
  mode: ReviewMode;
  scope: ReviewScope;
  findings: SeverityCounts;
  createdAt: string;
}

export interface Pull {
  repository: string;
  number: number;
  title: string;
  author: string;
  state: 'open' | 'closed';
  draft: boolean;
  merged: boolean;
  headSha: string;
  headRef: string;
  baseRef: string;
  url: string;
  openedAt: string | null;
  updatedAt: string;
  labels: Label[];
  lastReview: ReviewBrief | null;
}

export interface TokenCounts {
  input: number;
  output: number;
}

export interface Review {
  id: string;
  status: ReviewStatus;
  trigger: string;
  mode: ReviewMode;
  scope: ReviewScope;
  model: string;
  headSha: string;
  costUsd: number;
  tokens: TokenCounts;
  durationMs: number | null;
  createdAt: string;
  finishedAt: string | null;
  skipReason: SkipReason;
  error: string;
}

export interface Followup {
  id: string;
  commentId: number;
  repository: string;
  number: number;
  author: string;
  inline: boolean;
  path: string;
  line: number;
  status: FollowupStatus;
  reason: string;
  replyCommentId: number | null;
  model: string;
  createdAt: string;
}

export interface PullDetail {
  pull: Pull;
  reviews: Review[];
  followups: Followup[];
}

export interface PullRef {
  repository: string;
  number: number;
  title: string;
}

export interface ReviewInfo extends Review {
  pull: PullRef;
  scopeReason: string;
  mergeBaseSha: string;
  patchId: string;
  priorReviewId: string | null;
  cancelRequestedAt: string | null;
}

export interface Summary {
  take: string;
  praise: string[];
}

export interface Finding {
  id: string;
  path: string;
  line: number;
  endLine: number;
  severity: Severity;
  title: string;
  explanation: string;
  suggestedFix: string;
  replacement: string;
  agentPrompt: string;
  fingerprint: string;
  postedInline: boolean;
  forgeCommentId: number | null;
  createdAt: string;
}

export interface RunnerRun {
  id: string;
  phase: string;
  jobName: string;
  podName: string;
  nodeName: string;
  createdAt: string;
  scheduledAt: string | null;
  startedAt: string | null;
  finishedAt: string | null;
  heartbeatAt: string | null;
  exitCode: number | null;
  terminationReason: string;
  deadlineExceeded: boolean;
  error: string;
  logTail: string;
}

export interface Usage {
  input: number;
  cacheRead: number;
  cacheWrite: number;
  output: number;
}

export interface TimelineStep {
  index: number;
  tools: string[];
  durationMs: number;
  outputBytes: number;
  inputTokens: number;
  outputTokens: number;
}

export interface AgentRun {
  stopReason: string;
  steps: number;
  toolCalls: Record<string, number>;
  timeline: TimelineStep[];
  sources: string[];
  usage: Usage;
  costUsd: number;
  model: string;
  error: string;
  createdAt: string;
  // The submitted review JSON; null unless the agent submitted.
  result: unknown;
}

export interface UsageRow {
  role: string;
  model: string;
  upstream: string;
  inputTokens: number;
  outputTokens: number;
  costUsd: number;
  createdAt: string;
}

export interface Stage {
  stage: string;
  path: string;
  language: string;
  symbol: string;
  kind: string;
  scope: string;
  startLine: number;
  endLine: number;
  ref: string;
  bytes: number;
}

export interface RepoFile {
  path: string;
  size: number;
}

export interface ContextPack {
  headSha: string;
  baseSha: string;
  patchId: string;
  changedPaths: string[];
  deltaPaths: string[];
  priorHeadSha: string | null;
  stages: Stage[];
  repoNotes: string[];
  repoFiles: RepoFile[];
  createdAt: string;
}

export interface ReviewDetail {
  review: ReviewInfo;
  summary: Summary | null;
  findings: Finding[];
  runnerRun: RunnerRun | null;
  agentRun: AgentRun | null;
  usage: UsageRow[];
  contextPack: ContextPack | null;
}

export interface ReviewDiff {
  diff: string;
  deltaDiff: string;
}

export interface ContextChunk {
  stage: string;
  path: string;
  language: string;
  symbol: string;
  kind: string;
  scope: string;
  startLine: number;
  endLine: number;
  ref: string;
  text: string;
}

export interface ReviewRaw {
  repoFiles: Record<string, string>;
  stages: ContextChunk[];
  result: unknown;
  logTail: string;
}

export interface ToolDef {
  name: string;
  description: string;
  inputSchema: unknown;
}

export interface ToolCall {
  id: string;
  name: string;
  input: unknown;
}

export interface ToolResult {
  callId: string;
  content: string;
  isError: boolean;
  truncatedBytes: number;
}

export interface Message {
  role: MessageRole;
  text: string;
  toolCalls: ToolCall[];
  toolResults: ToolResult[];
}

export interface Response {
  text: string;
  toolCalls: ToolCall[];
  stop: string;
}

// system and tools are non-null only on a turn that changed them; reset
// says messages is the whole request rather than what is new since the
// previous turn of its run.
export interface Turn {
  index: number;
  id: string;
  kind: TranscriptKind;
  step: number;
  model: string;
  upstream: string;
  system: string | null;
  tools: ToolDef[] | null;
  reset: boolean;
  messagesFrom: number;
  messages: Message[];
  response: Response;
  usage: Usage;
  costUsd: number;
  durationMs: number;
  error: string;
  truncated: boolean;
  createdAt: string;
  runnerRunId: string;
}

export interface Transcript {
  system: string;
  tools: ToolDef[];
  turns: Turn[];
}

export interface UsagePoint {
  key: string;
  inputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  outputTokens: number;
  costUsd: number;
  calls: number;
}

export interface UsageSeries {
  group: UsageGroup;
  from: string;
  to: string;
  rows: UsagePoint[];
}

export interface JobArgs {
  repository: string;
  number: number;
  head: string;
  trigger: string;
  commentId: number;
}

export interface Job {
  id: number;
  kind: 'review' | 'followup' | 'index';
  state: JobState;
  attempt: number;
  maxAttempts: number;
  createdAt: string;
  scheduledAt: string;
  attemptedAt: string | null;
  finalizedAt: string | null;
  args: JobArgs;
  lastError: string;
}

// One server-sent event's data; the SSE event name is its kind, plus
// "resync" (data {}) when the client should refetch everything it shows.
export interface LiveEvent {
  kind: EventKind;
  tenant: string;
  id: string;
  reviewId: string | null;
}
