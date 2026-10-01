package webapi

import (
	"encoding/json"
	"time"

	"github.com/riverqueue/river/rivertype"

	"github.com/home-operations/kritika/internal/configfile"
	"github.com/home-operations/kritika/internal/model"
	"github.com/home-operations/kritika/internal/repoconfig"
	"github.com/home-operations/kritika/internal/review"
	"github.com/home-operations/kritika/internal/store"
	"github.com/home-operations/kritika/internal/transcript"
)

// Every JSON shape the dashboard API returns. internal/web/src/lib/types.ts
// mirrors these field for field; testdata/*.golden.json pins the names.

// Page is one page of a keyset-paginated list; NextCursor is null on the
// last page.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// User is the signed-in human.
type User struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	AvatarURL   string `json:"avatarUrl"`
}

// Me is who the session acts as: an admin, who reads and changes
// everything, or a member of Accounts, the slugs ("<forge>/<name>") of the
// accounts it may read.
type Me struct {
	User     User     `json:"user"`
	Admin    bool     `json:"admin"`
	Accounts []string `json:"accounts"`
}

// MonthUsage is an account's usage against its caps; a zero cap is unset.
type MonthUsage struct {
	Tokens         int64   `json:"tokens"`
	CostUSD        float64 `json:"costUsd"`
	TokensPerMonth int64   `json:"tokensPerMonth"`
	ReviewsToday   int64   `json:"reviewsToday"`
	ReviewsPerDay  int     `json:"reviewsPerDay"`
}

// AccountSummary is one row of the account list. Connection names the
// connection serving it.
type AccountSummary struct {
	Slug         string     `json:"slug"`
	Connection   string     `json:"connection"`
	Repositories int        `json:"repositories"`
	Reviews7d    int        `json:"reviews7d"`
	Usage        MonthUsage `json:"usage"`
}

// AdminAccount is one account as an admin sees it: Live is false for an
// entry of the instance spec that no connection serves, which Conflict
// explains.
type AdminAccount struct {
	AccountSummary
	Live     bool   `json:"live"`
	Conflict string `json:"conflict,omitempty"`
}

// InstanceSetting is one instance-wide setting as the admin console shows
// it, read-only: its value, and whether it comes from this process's
// environment, the configuration file, the dashboard or a built-in
// default. A secret shows only whether it is set.
type InstanceSetting struct {
	Section string            `json:"section"`
	Key     string            `json:"key"`
	Value   string            `json:"value"`
	Source  configfile.Source `json:"source"`
}

// CredentialsSet says which of a connection's secrets resolve to a
// value; the values themselves are never exposed.
type CredentialsSet struct {
	ClientID      bool `json:"clientId"`
	PrivateKey    bool `json:"privateKey"`
	WebhookSecret bool `json:"webhookSecret"`
}

// Connection is one GitHub App and the accounts it serves. HookPath is
// where its webhook arrives, under the dashboard's URL.
type Connection struct {
	Name        string           `json:"name"`
	Forge       configfile.Forge `json:"forge"`
	Accounts    []string         `json:"accounts"`
	Credentials CredentialsSet   `json:"credentials"`
	HookPath    string           `json:"hookPath"`
	// LastWebhookAt is when a webhook for the connection last passed
	// signature verification, to the minute; null when none ever has, and
	// kritika only polls it.
	LastWebhookAt *time.Time `json:"lastWebhookAt"`
	// LastUnsignedWebhookAt is when one last arrived with no signature, to
	// the minute, which a GitHub App with no webhook secret sends; null when
	// none has.
	LastUnsignedWebhookAt *time.Time `json:"lastUnsignedWebhookAt"`
}

// AccountDetail is one account's configuration and usage, and the
// connection serving it.
type AccountDetail struct {
	Slug       string            `json:"slug"`
	Connection Connection        `json:"connection"`
	Models     configfile.Models `json:"models"`
	Limits     configfile.Limits `json:"limits"`
	Filter     string            `json:"filter"`
	Usage      MonthUsage        `json:"usage"`
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
	ID            string `json:"id"`
	FullName      string `json:"fullName"`
	Enabled       bool   `json:"enabled"`
	ManagedBy     string `json:"managedBy"`
	DefaultBranch string `json:"defaultBranch"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	// TurnedOn is the choice an admin made in the dashboard, null while
	// the configuration decides whether the repository runs.
	TurnedOn   *bool      `json:"turnedOn"`
	Index      IndexState `json:"index"`
	LastReview *ReviewRef `json:"lastReview"`
}

// RepoSettings are a repository's settings as they resolve: the
// admin's, or with the in-repo .kritika.yaml applied (RepoConfig). The
// blocks are the configuration's own resolved types; durations are served
// in whole seconds.
type RepoSettings struct {
	Enabled        bool                     `json:"enabled"`
	Models         configfile.Models        `json:"models"`
	Filter         string                   `json:"filter"`
	Forks          bool                     `json:"forks"`
	Ignore         []string                 `json:"ignore"`
	SettleSeconds  int64                    `json:"settleSeconds"`
	MaxAutoReviews int                      `json:"maxAutoReviews"`
	MaxDeltaFiles  int                      `json:"maxDeltaFiles"`
	Review         configfile.Review        `json:"review"`
	Agent          configfile.AgentSettings `json:"agent"`
	Limits         configfile.Limits        `json:"limits"`
}

// RepoConfig is the repository's .kritika.yaml as the last review that ran
// read it, applied to the admin's settings as they are now.
type RepoConfig struct {
	ReviewID string `json:"reviewId"`
	// Commit is the merge base the review read the file at.
	Commit string `json:"commit"`
	// Found is false when there was no file there.
	Found bool `json:"found"`
	// Settings are the repository's settings with the file applied.
	Settings RepoSettings `json:"settings"`
	// Filter is the file's own filter, ANDed with the admin's.
	Filter string `json:"filter"`
	// Dropped are the file's values outside the admin's bounds; the
	// admin's value applies for each.
	Dropped []string `json:"dropped"`
	// Ignored is why the file was ignored as a whole, when it was.
	Ignored string `json:"ignored,omitempty"`
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
// Sources says, by the policy table's keys, which layer each of the
// admin's settings comes from: the defaults, the account (its entry or the
// repository's entry in it) or kritika's default. RepoConfig is null until a
// review has read the repository's .kritika.yaml.
type RepoDetail struct {
	Repository
	Settings   RepoSettings                 `json:"settings"`
	Sources    map[string]configfile.Source `json:"sources"`
	RepoConfig *RepoConfig                  `json:"repoConfig"`
	IndexRuns  []IndexRun                   `json:"indexRuns"`
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
	ID        string             `json:"id"`
	Status    store.ReviewStatus `json:"status"`
	Scope     review.Scope       `json:"scope"`
	Findings  SeverityCounts     `json:"findings"`
	CreatedAt time.Time          `json:"createdAt"`
}

// Pull is one pull request.
type Pull struct {
	Repository string       `json:"repository"`
	Number     int          `json:"number"`
	Title      string       `json:"title"`
	Author     string       `json:"author"`
	State      string       `json:"state"`
	Draft      bool         `json:"draft"`
	Fork       bool         `json:"fork"`
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

// PullRef names a pull request, and where it is on the forge.
type PullRef struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
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
	Category       review.Category `json:"category"`
	Title          string          `json:"title"`
	Explanation    string          `json:"explanation"`
	SuggestedFix   string          `json:"suggestedFix"`
	Replacement    string          `json:"replacement"`
	AgentPrompt    string          `json:"agentPrompt"`
	Fingerprint    string          `json:"fingerprint"`
	PostedInline   bool            `json:"postedInline"`
	ForgeCommentID *int64          `json:"forgeCommentId"`
	CreatedAt      time.Time       `json:"createdAt"`
	// ReactionsUp and ReactionsDown count the 👍 and 👎 on its inline
	// comment, as the poller last read them.
	ReactionsUp   int `json:"reactionsUp"`
	ReactionsDown int `json:"reactionsDown"`
	// Rules are the ids of the review rules it enforces.
	Rules []string `json:"rules"`
}

// AccountFinding is one finding of a pull request, however many of its
// reviews reported it, as the latest of them did. Status is whether a
// later review no longer reported it, or a maintainer dismissed it, with
// DismissReason the reason they gave.
type AccountFinding struct {
	Finding
	ReviewID      string              `json:"reviewId"`
	Pull          PullRef             `json:"pull"`
	Status        store.FindingStatus `json:"status"`
	DismissReason string              `json:"dismissReason"`
	FirstSeenAt   time.Time           `json:"firstSeenAt"`
	LastSeenAt    time.Time           `json:"lastSeenAt"`
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

// AgentRun is a review's tool loop. Result is the submitted
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
// since the prior review's head. Swept says the retention sweep has
// emptied both.
type ReviewDiff struct {
	Diff      string `json:"diff"`
	DeltaDiff string `json:"deltaDiff"`
	Swept     bool   `json:"swept"`
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

// UsageSeries is the account's usage in [From, To) grouped by Group.
type UsageSeries struct {
	Group store.UsageGroup `json:"group"`
	From  time.Time        `json:"from"`
	To    time.Time        `json:"to"`
	Rows  []UsagePoint     `json:"rows"`
}

// RuleKind is what a rule is to a review: a check written in the
// configuration, as text or a file, or a context file that explains the
// code.
type RuleKind string

// Rule kinds.
const (
	RuleWritten RuleKind = "rule"
	RuleContext RuleKind = "context"
)

// RuleSource is where a rule is set: a layer of the configuration, as
// configfile.Source names it, RuleFromEntry, an account's entry for the
// repository, or RuleFromRepository, the repository's own .kritika.yaml.
type RuleSource string

// Rule sources beyond the configuration's layers.
const (
	RuleFromRepository RuleSource = "repository"
	RuleFromEntry      RuleSource = "entry"
)

// Rule is one written rule or file reviews read, with where it is set,
// the paths it applies to (every change when empty), and the running
// repositories whose reviews read it. ID is a written rule's, with its
// Text or, for a file rule, its Path, and WhenExpr, the CEL expression
// over the pull request it applies only when true of; Path and
// Description are a context file's. Findings and Addressed are a written
// rule's too: its repositories' findings that cite its id, once per pull
// request as the findings list counts them, and how many of those were
// addressed.
type Rule struct {
	Kind         RuleKind   `json:"kind"`
	ID           string     `json:"id"`
	Text         string     `json:"text"`
	Path         string     `json:"path"`
	Description  string     `json:"description"`
	Paths        []string   `json:"paths"`
	WhenExpr     string     `json:"whenExpr"`
	Source       RuleSource `json:"source"`
	Repositories []string   `json:"repositories"`
	Findings     int        `json:"findings"`
	Addressed    int        `json:"addressed"`
}

// AnalyticsTotals is what the account's reviews came to over a window:
// completed reviews and the pull requests they reviewed, failed reviews,
// the findings first reported in it and how many were addressed since,
// the reactions to those posted inline, spend, the median time a
// completed review took, and the median time from opening to merging.
type AnalyticsTotals struct {
	PullRequests int            `json:"pullRequests"`
	Reviews      int            `json:"reviews"`
	Failed       int            `json:"failed"`
	Findings     SeverityCounts `json:"findings"`
	// Categories counts the same findings by category, every category
	// present.
	Categories     map[review.Category]int `json:"categories"`
	Addressed      int                     `json:"addressed"`
	ReactionsUp    int                     `json:"reactionsUp"`
	ReactionsDown  int                     `json:"reactionsDown"`
	CostUSD        float64                 `json:"costUsd"`
	MedianReviewMs *int64                  `json:"medianReviewMs"`
	MedianMergeMs  *int64                  `json:"medianMergeMs"`
}

// AnalyticsPoint is one bucket of the series, keyed by its first date.
type AnalyticsPoint struct {
	Key      string         `json:"key"`
	Reviews  int            `json:"reviews"`
	Findings SeverityCounts `json:"findings"`
	CostUSD  float64        `json:"costUsd"`
}

// RepoActivity is what one repository's reviews came to over the window.
type RepoActivity struct {
	Repository string         `json:"repository"`
	Reviews    int            `json:"reviews"`
	Findings   SeverityCounts `json:"findings"`
	Addressed  int            `json:"addressed"`
}

// Analytics is the account's reviews in [From, To), against the window of
// the same length before it, bucketed by Group, and the repositories with
// the most reviews.
type Analytics struct {
	Group        store.AnalyticsGroup `json:"group"`
	From         time.Time            `json:"from"`
	To           time.Time            `json:"to"`
	Current      AnalyticsTotals      `json:"current"`
	Previous     AnalyticsTotals      `json:"previous"`
	Series       []AnalyticsPoint     `json:"series"`
	Repositories []RepoActivity       `json:"repositories"`
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

// Job is one River job of the account.
type Job struct {
	ID          int64              `json:"id"`
	Kind        string             `json:"kind"`
	State       rivertype.JobState `json:"state"`
	Attempt     int                `json:"attempt"`
	MaxAttempts int                `json:"maxAttempts"`
	CreatedAt   time.Time          `json:"createdAt"`
	ScheduledAt time.Time          `json:"scheduledAt"`
	AttemptedAt *time.Time         `json:"attemptedAt"`
	FinalizedAt *time.Time         `json:"finalizedAt"`
	Args        JobArgs            `json:"args"`
	LastError   string             `json:"lastError"`
}

// Event is one server-sent event's data: a row of Kind changed in the
// account with this slug.
type Event struct {
	Kind     store.EventKind `json:"kind"`
	Account  string          `json:"account"`
	ID       string          `json:"id"`
	ReviewID *string         `json:"reviewId"`
}

// ErrorBody is every API error.
type ErrorBody struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}
