package webapi

import (
	"encoding/json"
	"time"

	"github.com/home-operations/kritik/internal/auth"
	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/model"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
	"github.com/home-operations/kritik/internal/store"
	"github.com/home-operations/kritik/internal/transcript"
)

// Every JSON shape the dashboard API returns. internal/web/src/lib/types.ts
// mirrors these field for field; testdata/*.golden.json pins the names.

// Page is one page of a keyset-paginated list; NextCursor is null on the
// last page.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// Account is the signed-in human.
type Account struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	AvatarURL   string `json:"avatarUrl"`
}

// TenantMembership is a tenant the principal may read and its role there.
type TenantMembership struct {
	Slug      string            `json:"slug"`
	Role      auth.Role         `json:"role"`
	ManagedBy configfile.Origin `json:"managedBy"`
}

// Me is who the session acts as.
type Me struct {
	Account  Account            `json:"account"`
	Operator bool               `json:"operator"`
	Tenants  []TenantMembership `json:"tenants"`
}

// MonthUsage is a tenant's usage against its caps; a zero cap is unset.
type MonthUsage struct {
	Tokens         int64   `json:"tokens"`
	CostUSD        float64 `json:"costUsd"`
	TokensPerMonth int64   `json:"tokensPerMonth"`
	ReviewsToday   int64   `json:"reviewsToday"`
	ReviewsPerDay  int     `json:"reviewsPerDay"`
}

// TenantSummary is one row of the tenant list.
type TenantSummary struct {
	Slug          string            `json:"slug"`
	ManagedBy     configfile.Origin `json:"managedBy"`
	Role          auth.Role         `json:"role"`
	Installations int               `json:"installations"`
	Repositories  int               `json:"repositories"`
	Reviews7d     int               `json:"reviews7d"`
	Usage         MonthUsage        `json:"usage"`
}

// OperatorTenant is one tenant as an operator sees it: Live is false for
// a dashboard tenant stored but not part of the running configuration,
// because it does not validate or has not been merged yet.
type OperatorTenant struct {
	TenantSummary
	Live     bool  `json:"live"`
	Revision int64 `json:"revision"`
}

// CredentialKind is how an installation authenticates to its forge.
type CredentialKind string

// Credential kinds, as installations.credential_kind spells them.
const (
	CredentialApp   CredentialKind = "app"
	CredentialToken CredentialKind = "token"
)

// CredentialsSet says which of an installation's secrets resolve to a
// value; the values themselves are never exposed.
type CredentialsSet struct {
	ClientID      bool `json:"clientId"`
	PrivateKey    bool `json:"privateKey"`
	Token         bool `json:"token"`
	GitToken      bool `json:"gitToken"`
	WebhookSecret bool `json:"webhookSecret"`
}

// Installation is one bot on one forge account. HookPath is relative to
// the ingest endpoint, whose origin the dashboard does not know.
type Installation struct {
	Name           string           `json:"name"`
	Forge          configfile.Forge `json:"forge"`
	Host           string           `json:"host"`
	Account        string           `json:"account"`
	CredentialKind CredentialKind   `json:"credentialKind"`
	Credentials    CredentialsSet   `json:"credentials"`
	HookPath       string           `json:"hookPath"`
}

// Models names the review and fallback models, "provider/model".
type Models struct {
	Review   configfile.ModelRef `json:"review"`
	Fallback configfile.ModelRef `json:"fallback"`
}

// Limits are resolved limits; zero is unset.
type Limits struct {
	Concurrency    int   `json:"concurrency"`
	ReviewsPerDay  int   `json:"reviewsPerDay"`
	TokensPerMonth int64 `json:"tokensPerMonth"`
}

// TenantDetail is one tenant's configuration and usage.
type TenantDetail struct {
	Slug          string            `json:"slug"`
	ManagedBy     configfile.Origin `json:"managedBy"`
	Role          auth.Role         `json:"role"`
	Installations []Installation    `json:"installations"`
	Models        Models            `json:"models"`
	Limits        Limits            `json:"limits"`
	Filter        string            `json:"filter"`
	Usage         MonthUsage        `json:"usage"`
}

// IndexState is a repository's embedding index: the active generation and
// the newest run. Empty strings and nulls mean none.
type IndexState struct {
	ActiveCommit  string               `json:"activeCommit"`
	ActiveAt      *time.Time           `json:"activeAt"`
	LastRunStatus store.IndexRunStatus `json:"lastRunStatus"`
	LastRunAt     *time.Time           `json:"lastRunAt"`
}

// ReviewRef is a review's id, status and start.
type ReviewRef struct {
	ID        string             `json:"id"`
	Status    store.ReviewStatus `json:"status"`
	CreatedAt time.Time          `json:"createdAt"`
}

// Repository is one row of the repository list.
type Repository struct {
	ID            string     `json:"id"`
	FullName      string     `json:"fullName"`
	Installation  string     `json:"installation"`
	Enabled       bool       `json:"enabled"`
	ManagedBy     string     `json:"managedBy"`
	DefaultBranch string     `json:"defaultBranch"`
	Index         IndexState `json:"index"`
	LastReview    *ReviewRef `json:"lastReview"`
}

// AgentLimits are an agentic review's resolved limits.
type AgentLimits struct {
	MaxSteps              int      `json:"maxSteps"`
	MaxToolOutputBytes    int      `json:"maxToolOutputBytes"`
	MaxTokens             int64    `json:"maxTokens"`
	TimeoutSeconds        int64    `json:"timeoutSeconds"`
	Commands              []string `json:"commands"`
	CommandTimeoutSeconds int64    `json:"commandTimeoutSeconds"`
}

// ReviewBlock is a repository's review instructions and options.
type ReviewBlock struct {
	Instructions        []string `json:"instructions"`
	RequireSuggestedFix bool     `json:"requireSuggestedFix"`
}

// RepoSettings are a repository's effective settings as the file and the
// dashboard resolve them; the in-repo .kritik.yaml is not included.
type RepoSettings struct {
	Enabled       bool                  `json:"enabled"`
	Mode          configfile.ReviewMode `json:"mode"`
	Models        Models                `json:"models"`
	Filter        string                `json:"filter"`
	Forks         bool                  `json:"forks"`
	Ignore        []string              `json:"ignore"`
	SettleSeconds int64                 `json:"settleSeconds"`
	MaxDeltaFiles int                   `json:"maxDeltaFiles"`
	Review        ReviewBlock           `json:"review"`
	Agent         AgentLimits           `json:"agent"`
	Limits        Limits                `json:"limits"`
}

// IndexRun is one index generation or step.
type IndexRun struct {
	ID         string               `json:"id"`
	Repository string               `json:"repository"`
	CommitSHA  string               `json:"commitSha"`
	BaseSHA    string               `json:"baseSha"`
	EmbedModel string               `json:"embedModel"`
	Mode       string               `json:"mode"`
	Status     store.IndexRunStatus `json:"status"`
	Trigger    string               `json:"trigger"`
	ChunkCount int                  `json:"chunkCount"`
	Error      string               `json:"error"`
	CreatedAt  time.Time            `json:"createdAt"`
	FinishedAt *time.Time           `json:"finishedAt"`
}

// RepoDetail is one repository, its settings and recent index runs.
type RepoDetail struct {
	Repository
	Settings  RepoSettings `json:"settings"`
	IndexRuns []IndexRun   `json:"indexRuns"`
}

// Label is a pull request label.
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// SeverityCounts counts findings by severity.
type SeverityCounts struct {
	Blocking  int `json:"blocking"`
	Important int `json:"important"`
	Nit       int `json:"nit"`
}

// ReviewBrief is a pull request's newest review.
type ReviewBrief struct {
	ID        string                `json:"id"`
	Status    store.ReviewStatus    `json:"status"`
	Mode      configfile.ReviewMode `json:"mode"`
	Scope     review.Scope          `json:"scope"`
	Findings  SeverityCounts        `json:"findings"`
	CreatedAt time.Time             `json:"createdAt"`
}

// Pull is one pull request.
type Pull struct {
	Repository string       `json:"repository"`
	Number     int          `json:"number"`
	Title      string       `json:"title"`
	Author     string       `json:"author"`
	State      string       `json:"state"`
	Draft      bool         `json:"draft"`
	Merged     bool         `json:"merged"`
	HeadSHA    string       `json:"headSha"`
	HeadRef    string       `json:"headRef"`
	BaseRef    string       `json:"baseRef"`
	URL        string       `json:"url"`
	OpenedAt   *time.Time   `json:"openedAt"`
	UpdatedAt  time.Time    `json:"updatedAt"`
	Labels     []Label      `json:"labels"`
	LastReview *ReviewBrief `json:"lastReview"`
}

// TokenCounts are input and output tokens.
type TokenCounts struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

// Review is one review pass as lists show it. DurationMs is null while
// the review runs.
type Review struct {
	ID         string                `json:"id"`
	Status     store.ReviewStatus    `json:"status"`
	Trigger    string                `json:"trigger"`
	Mode       configfile.ReviewMode `json:"mode"`
	Scope      review.Scope          `json:"scope"`
	Model      string                `json:"model"`
	HeadSHA    string                `json:"headSha"`
	CostUSD    float64               `json:"costUsd"`
	Tokens     TokenCounts           `json:"tokens"`
	DurationMs *int64                `json:"durationMs"`
	CreatedAt  time.Time             `json:"createdAt"`
	FinishedAt *time.Time            `json:"finishedAt"`
	SkipReason repoconfig.SkipReason `json:"skipReason"`
	Error      string                `json:"error"`
}

// Followup is one @-mention of the bot and what came of it.
type Followup struct {
	ID             string               `json:"id"`
	CommentID      int64                `json:"commentId"`
	Repository     string               `json:"repository"`
	Number         int                  `json:"number"`
	Author         string               `json:"author"`
	Inline         bool                 `json:"inline"`
	Path           string               `json:"path"`
	Line           int                  `json:"line"`
	Status         store.FollowupStatus `json:"status"`
	Reason         string               `json:"reason"`
	ReplyCommentID *int64               `json:"replyCommentId"`
	Model          string               `json:"model"`
	CreatedAt      time.Time            `json:"createdAt"`
}

// PullDetail is a pull request with every review and follow-up.
type PullDetail struct {
	Pull      Pull       `json:"pull"`
	Reviews   []Review   `json:"reviews"`
	Followups []Followup `json:"followups"`
}

// PullRef names a pull request.
type PullRef struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
}

// ReviewInfo is a review row in full.
type ReviewInfo struct {
	Review
	Pull              PullRef    `json:"pull"`
	ScopeReason       string     `json:"scopeReason"`
	MergeBaseSHA      string     `json:"mergeBaseSha"`
	PatchID           string     `json:"patchId"`
	PriorReviewID     *string    `json:"priorReviewId"`
	CancelRequestedAt *time.Time `json:"cancelRequestedAt"`
}

// Summary is a review's overall take and praise.
type Summary struct {
	Take   string   `json:"take"`
	Praise []string `json:"praise"`
}

// Finding is one finding in full.
type Finding struct {
	ID             string          `json:"id"`
	Path           string          `json:"path"`
	Line           int             `json:"line"`
	EndLine        int             `json:"endLine"`
	Severity       review.Severity `json:"severity"`
	Title          string          `json:"title"`
	Explanation    string          `json:"explanation"`
	SuggestedFix   string          `json:"suggestedFix"`
	Replacement    string          `json:"replacement"`
	AgentPrompt    string          `json:"agentPrompt"`
	Fingerprint    string          `json:"fingerprint"`
	PostedInline   bool            `json:"postedInline"`
	ForgeCommentID *int64          `json:"forgeCommentId"`
	CreatedAt      time.Time       `json:"createdAt"`
}

// RunnerRun is the Kubernetes Job that prepared a review.
type RunnerRun struct {
	ID                string     `json:"id"`
	Phase             string     `json:"phase"`
	JobName           string     `json:"jobName"`
	PodName           string     `json:"podName"`
	NodeName          string     `json:"nodeName"`
	CreatedAt         time.Time  `json:"createdAt"`
	ScheduledAt       *time.Time `json:"scheduledAt"`
	StartedAt         *time.Time `json:"startedAt"`
	FinishedAt        *time.Time `json:"finishedAt"`
	HeartbeatAt       *time.Time `json:"heartbeatAt"`
	ExitCode          *int       `json:"exitCode"`
	TerminationReason string     `json:"terminationReason"`
	DeadlineExceeded  bool       `json:"deadlineExceeded"`
	Error             string     `json:"error"`
	LogTail           string     `json:"logTail"`
}

// Usage counts one or more model calls' tokens.
type Usage struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Output     int64 `json:"output"`
}

// TimelineStep is one agent step.
type TimelineStep struct {
	Index        int      `json:"index"`
	Tools        []string `json:"tools"`
	DurationMs   int64    `json:"durationMs"`
	OutputBytes  int      `json:"outputBytes"`
	InputTokens  int64    `json:"inputTokens"`
	OutputTokens int64    `json:"outputTokens"`
}

// AgentRun is an agentic review's tool loop. Result is the submitted
// review JSON, null unless the agent submitted.
type AgentRun struct {
	StopReason string          `json:"stopReason"`
	Steps      int             `json:"steps"`
	ToolCalls  map[string]int  `json:"toolCalls"`
	Timeline   []TimelineStep  `json:"timeline"`
	Sources    []string        `json:"sources"`
	Usage      Usage           `json:"usage"`
	CostUSD    float64         `json:"costUsd"`
	Model      string          `json:"model"`
	Error      string          `json:"error"`
	CreatedAt  time.Time       `json:"createdAt"`
	Result     json.RawMessage `json:"result"`
}

// UsageRow is one usage row charged to a review.
type UsageRow struct {
	Role         string    `json:"role"`
	Model        string    `json:"model"`
	Upstream     string    `json:"upstream"`
	InputTokens  int64     `json:"inputTokens"`
	OutputTokens int64     `json:"outputTokens"`
	CostUSD      float64   `json:"costUsd"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Stage is one context chunk without its text.
type Stage struct {
	Stage     string `json:"stage"`
	Path      string `json:"path"`
	Language  string `json:"language"`
	Symbol    string `json:"symbol"`
	Kind      string `json:"kind"`
	Scope     string `json:"scope"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Ref       string `json:"ref"`
	Bytes     int    `json:"bytes"`
}

// RepoFile is one repository file the context pack read, by size.
type RepoFile struct {
	Path string `json:"path"`
	Size int    `json:"size"`
}

// ContextPack is what a review's Job gathered, without diff bodies,
// stage texts or file contents.
type ContextPack struct {
	HeadSHA      string     `json:"headSha"`
	BaseSHA      string     `json:"baseSha"`
	PatchID      string     `json:"patchId"`
	ChangedPaths []string   `json:"changedPaths"`
	DeltaPaths   []string   `json:"deltaPaths"`
	PriorHeadSHA *string    `json:"priorHeadSha"`
	Stages       []Stage    `json:"stages"`
	RepoNotes    []string   `json:"repoNotes"`
	RepoFiles    []RepoFile `json:"repoFiles"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// ReviewDetail is everything recorded about one review but its bodies.
type ReviewDetail struct {
	Review      ReviewInfo   `json:"review"`
	Summary     *Summary     `json:"summary"`
	Findings    []Finding    `json:"findings"`
	RunnerRun   *RunnerRun   `json:"runnerRun"`
	AgentRun    *AgentRun    `json:"agentRun"`
	Usage       []UsageRow   `json:"usage"`
	ContextPack *ContextPack `json:"contextPack"`
}

// ReviewDiff is a review's diff and, for an incremental one, the diff
// since the prior review's head.
type ReviewDiff struct {
	Diff      string `json:"diff"`
	DeltaDiff string `json:"deltaDiff"`
}

// ContextChunk is one context chunk with its text.
type ContextChunk struct {
	Stage     string `json:"stage"`
	Path      string `json:"path"`
	Language  string `json:"language"`
	Symbol    string `json:"symbol"`
	Kind      string `json:"kind"`
	Scope     string `json:"scope"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Ref       string `json:"ref"`
	Text      string `json:"text"`
}

// ReviewRaw is the review's inputs and output as stored: the repository
// files and context the runner gathered, the agent's submitted review
// (null for a review that did not submit one), and the runner's log tail.
type ReviewRaw struct {
	RepoFiles map[string]string `json:"repoFiles"`
	Stages    []ContextChunk    `json:"stages"`
	Result    json.RawMessage   `json:"result"`
	LogTail   string            `json:"logTail"`
}

// ToolDef is a tool a model was offered.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolCall is a tool call a model made.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is what a tool call returned.
type ToolResult struct {
	CallID         string `json:"callId"`
	Content        string `json:"content"`
	IsError        bool   `json:"isError"`
	TruncatedBytes int    `json:"truncatedBytes"`
}

// Message is one request message.
type Message struct {
	Role        model.Role   `json:"role"`
	Text        string       `json:"text"`
	ToolCalls   []ToolCall   `json:"toolCalls"`
	ToolResults []ToolResult `json:"toolResults"`
}

// Response is what the model answered.
type Response struct {
	Text      string           `json:"text"`
	ToolCalls []ToolCall       `json:"toolCalls"`
	Stop      model.StopReason `json:"stop"`
}

// Turn is one model call: the messages new in its request from index
// MessagesFrom, and the answer. System and Tools are set when the call
// changed them; Reset says Messages is the whole request.
type Turn struct {
	Index        int             `json:"index"`
	ID           string          `json:"id"`
	Kind         transcript.Kind `json:"kind"`
	Step         int             `json:"step"`
	Model        string          `json:"model"`
	Upstream     string          `json:"upstream"`
	System       *string         `json:"system"`
	Tools        []ToolDef       `json:"tools"`
	Reset        bool            `json:"reset"`
	MessagesFrom int             `json:"messagesFrom"`
	Messages     []Message       `json:"messages"`
	Response     Response        `json:"response"`
	Usage        Usage           `json:"usage"`
	CostUSD      float64         `json:"costUsd"`
	DurationMs   int64           `json:"durationMs"`
	Error        string          `json:"error"`
	Truncated    bool            `json:"truncated"`
	CreatedAt    time.Time       `json:"createdAt"`
	RunnerRunID  string          `json:"runnerRunId"`
}

// Transcript is a set of model calls: the system prompt and tools the
// first carried, then one turn per call.
type Transcript struct {
	System string    `json:"system"`
	Tools  []ToolDef `json:"tools"`
	Turns  []Turn    `json:"turns"`
}

// UsagePoint is one key of a usage series.
type UsagePoint struct {
	Key              string  `json:"key"`
	InputTokens      int64   `json:"inputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CostUSD          float64 `json:"costUsd"`
	Calls            int64   `json:"calls"`
}

// UsageSeries is the tenant's usage in [From, To) grouped by Group.
type UsageSeries struct {
	Group store.UsageGroup `json:"group"`
	From  time.Time        `json:"from"`
	To    time.Time        `json:"to"`
	Rows  []UsagePoint     `json:"rows"`
}

// JobArgs are the parts of a job's arguments the queue shows; fields a
// kind does not carry are zero.
type JobArgs struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	Head       string `json:"head"`
	Trigger    string `json:"trigger"`
	CommentID  int64  `json:"commentId"`
}

// Job is one River job of the tenant.
type Job struct {
	ID          int64          `json:"id"`
	Kind        string         `json:"kind"`
	State       store.JobState `json:"state"`
	Attempt     int            `json:"attempt"`
	MaxAttempts int            `json:"maxAttempts"`
	CreatedAt   time.Time      `json:"createdAt"`
	ScheduledAt time.Time      `json:"scheduledAt"`
	AttemptedAt *time.Time     `json:"attemptedAt"`
	FinalizedAt *time.Time     `json:"finalizedAt"`
	Args        JobArgs        `json:"args"`
	LastError   string         `json:"lastError"`
}

// Event is one server-sent event's data: a row of Kind changed in the
// tenant with this slug.
type Event struct {
	Kind     store.EventKind `json:"kind"`
	Tenant   string          `json:"tenant"`
	ID       string          `json:"id"`
	ReviewID *string         `json:"reviewId"`
}

// ErrorBody is every API error.
type ErrorBody struct {
	Code    ErrorCode       `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}
