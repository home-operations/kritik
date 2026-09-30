// Mirrors internal/webapi/types.go field for field; the Go side pins the
// JSON names in internal/webapi/testdata/*.golden.json. Timestamps are
// RFC 3339 strings; null means "none".

// Where a connection is declared: the configuration file (or its
// environment), or the dashboard.
export type ConnectionOrigin = 'file' | 'dashboard';

// A person signed in to the dashboard.
export interface User {
  id: string;
  displayName: string;
  email: string;
  avatarUrl: string;
}

// admin administers the instance and every account; accounts are the slugs
// ("<forge>/<name>") of the accounts the viewer may read.
export interface Me {
  user: User;
  admin: boolean;
  accounts: string[];
}

// local is the admin's username and password form, posted to /auth/local.
export type SignInProviderType = 'local' | 'oidc' | 'github';

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

// unauthenticated and csrf come from the auth middleware in front of every
// API route: no valid session, and a state change that is not same-origin.
export type ErrorCode = 'not_found' | 'bad_request' | 'invalid_cursor' | 'ambiguous' | 'internal' | 'unauthenticated' | 'csrf';

export interface ErrorBody {
  code: ErrorCode | string;
  message: string;
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
export type Forge = 'github';
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

// connection names the connection serving the account.
export interface AccountSummary {
  slug: string;
  connection: string;
  repositories: number;
  reviews7d: number;
  usage: MonthUsage;
}

// live is false for an entry of the instance spec no connection serves,
// which conflict explains.
export interface AdminAccount extends AccountSummary {
  live: boolean;
  conflict?: string;
}

export interface CredentialsSet {
  clientId: boolean;
  privateKey: boolean;
  webhookSecret: boolean;
}

export interface Connection {
  name: string;
  forge: Forge;
  managedBy: ConnectionOrigin;
  accounts: string[];
  credentials: CredentialsSet;
  hookPath: string;
  // lastWebhookAt is null until a webhook for the connection reaches
  // kritik; until then kritik only polls it.
  lastWebhookAt: string | null;
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

export interface AccountDetail {
  slug: string;
  connection: Connection;
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
  enabled: boolean;
  managedBy: 'dashboard' | 'forge';
  defaultBranch: string;
  archived: boolean;
  fork: boolean;
  // turnedOn is the choice an admin made in the dashboard, null while the
  // configuration decides.
  turnedOn: boolean | null;
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

export interface ContextFile {
  path: string;
  description: string;
  paths?: string[];
}

export interface ReviewBlock {
  requireSuggestedFix: boolean;
  templates: { summary?: string; inline?: string };
  inlineComments: boolean;
  context: ContextFile[];
  feedback: Feedback;
  agentFiles: boolean;
}

// How much a review says: anything a maintainer could act on, the same
// with nits kept to the summary, or only bugs, risks and breaking changes.
export type Feedback = 'detailed' | 'standard' | 'minimal';

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

// defaults is the instance spec's defaults, account an account's entry in
// it, dashboard the instance spec itself.
export type ConfigSource = 'default' | 'env' | 'file' | 'defaults' | 'account' | 'repository';

// One instance-wide setting, read-only in the admin console: a secret
// shows only whether it is set.
export interface InstanceSetting {
  section: string;
  key: string;
  value: string;
  source: ConfigSource;
}

// The repository's .kritik.yaml as the last review that ran read it, at
// its merge base, applied to the admin's settings as they are now.
export interface RepoConfig {
  reviewId: string;
  commit: string;
  found: boolean;
  settings: RepoSettings;
  filter: string;
  dropped: string[];
  ignored?: string;
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
  // Where each of the admin's settings comes from, by policy key.
  sources: Record<string, ConfigSource>;
  repoConfig: RepoConfig | null;
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
  // fork is whether the head is in another repository: such a pull request
  // is reviewed when a maintainer asks.
  fork: boolean;
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
  url: string;
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
  reactionsUp: number;
  reactionsDown: number;
  // rules are the ids of the review rules it enforces.
  rules: string[];
}

export type AnalyticsGroup = 'day' | 'week' | 'month';

export interface AnalyticsTotals {
  pullRequests: number;
  reviews: number;
  failed: number;
  findings: SeverityCounts;
  addressed: number;
  reactionsUp: number;
  reactionsDown: number;
  costUsd: number;
  medianReviewMs: number | null;
  medianMergeMs: number | null;
}

export interface AnalyticsPoint {
  key: string;
  reviews: number;
  findings: SeverityCounts;
  costUsd: number;
}

export interface RepoActivity {
  repository: string;
  reviews: number;
  findings: SeverityCounts;
  addressed: number;
}

export interface Analytics {
  group: AnalyticsGroup;
  from: string;
  to: string;
  current: AnalyticsTotals;
  previous: AnalyticsTotals;
  series: AnalyticsPoint[];
  repositories: RepoActivity[];
}

export type RuleKind = 'rule' | 'context';
// entry is an account's entry for the repository; repository is the
// repository's own .kritik.yaml.
export type RuleSource = 'default' | 'env' | 'file' | 'defaults' | 'account' | 'entry' | 'repository';

// Rule is one written rule or file reviews read, with where it is set, the
// paths it applies to (every change when empty), and the repositories that
// read it. id, text and whenExpr, the CEL expression over the pull request
// it applies only when true of, are a written rule's, path and description
// a file's.
export interface Rule {
  kind: RuleKind;
  id: string;
  text: string;
  path: string;
  description: string;
  paths: string[];
  whenExpr: string;
  source: RuleSource;
  repositories: string[];
  // findings and addressed are a written rule's: its repositories'
  // findings that cite its id, and how many of those were addressed.
  findings: number;
  addressed: number;
}

export type FindingStatus = 'open' | 'addressed';

// AccountFinding is one finding of a pull request, however many of its
// reviews reported it, as the latest of them did.
export interface AccountFinding extends Finding {
  reviewId: string;
  pull: PullRef;
  status: FindingStatus;
  firstSeenAt: string;
  lastSeenAt: string;
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
  account: string;
  id: string;
  reviewId: string | null;
}

// The management API: actions, connections and the audit log.
// ErrorBody.code may also be one of these.
export type ManagementErrorCode =
  | 'forbidden'
  | 'no_head'
  | 'not_cancelable'
  | 'actions_disabled'
  | 'already_queued'
  | 'forge_error'
  | 'installation_served';

export interface Meta {
  version: string;
  webUrl: string;
}

export interface Accepted {
  jobId?: number;
}

// One account a connection's GitHub App is installed on. served is whether
// the connection lists the account; kritik reviews nothing on one it does
// not, and an admin may uninstall the App there.
export interface AppInstallation {
  id: number;
  account: string;
  accountType: string;
  allRepositories: boolean;
  suspended: boolean;
  served: boolean;
  url?: string;
}

// How far the instance is from reviewing: what the Configuration page's
// checklist shows.
export interface SetupStatus {
  webUrl: string;
  // Where each connection's webhook goes, its name appended.
  hooksUrl: string;
  connections: string[];
  // defaults.models.review, '' when unset.
  reviewModel: string;
  embedding: boolean;
}

export interface RegisterResult {
  added: number;
}

export type AuditAction =
  | 'config.update'
  | 'account.update'
  | 'app.create'
  | 'app.uninstall'
  | 'review.rerun'
  | 'review.cancel'
  | 'repo.reindex'
  | 'repo.turn_on'
  | 'repo.turn_off';

// Turns a repository on, or off.
export interface TurnOnRequest {
  on: boolean;
}

export interface AuditEvent {
  id: string;
  at: string;
  actor: User | null;
  account: string;
  action: AuditAction;
  target: string;
  detail: Record<string, unknown>;
}
