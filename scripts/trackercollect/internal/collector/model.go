package collector

import "time"

const SchemaVersion = 1

type Repository struct {
	Host  string
	Owner string
	Name  string
}

func (r Repository) NameWithOwner() string { return r.Owner + "/" + r.Name }
func (r Repository) String() string        { return r.Host + "/" + r.NameWithOwner() }

type Config struct {
	Repository  Repository
	PullRequest int
	OutputDir   string
	Direct      bool
	PageSize    int
	PageCap     int
}

type Snapshot struct {
	SchemaVersion           int                 `json:"schema_version"`
	Repository              string              `json:"repository"`
	Collection              CollectionStamp     `json:"collection"`
	MergedPullRequest       MergedPullRequest   `json:"merged_pull_request"`
	PinnedIssues            []PinnedIssue       `json:"pinned_issues"`
	OpenWaveTitleMatchCount int                 `json:"open_wave_title_match_count"`
	OpenIssueInventory      []IssueIdentity     `json:"open_issue_inventory"`
	ContainingTrackers      []ContainingTracker `json:"containing_trackers"`
	OpenPullRequests        []OpenPullRequest   `json:"open_pull_requests"`
	MarkerComments          []MarkerComment     `json:"marker_comments"`
	Ambiguities             []Ambiguity         `json:"ambiguities"`
}

type CollectionStamp struct {
	DefaultBranchHeadSHA string    `json:"default_branch_head_sha"`
	StartedAt            time.Time `json:"started_at"`
	CompletedAt          time.Time `json:"completed_at"`
}

type ForgeStamp struct {
	NodeID     string `json:"node_id"`
	DatabaseID int64  `json:"database_id"`
	UpdatedAt  string `json:"updated_at"`
	BodySHA256 string `json:"body_sha256,omitempty"`
}

type IssueSummary struct {
	Number int        `json:"number"`
	Title  string     `json:"title"`
	State  string     `json:"state"`
	Stamp  ForgeStamp `json:"stamp"`
}

type IssueIdentity struct {
	Number int        `json:"number"`
	Stamp  ForgeStamp `json:"stamp"`
}

type MergedPullRequest struct {
	Number         int            `json:"number"`
	Title          string         `json:"title"`
	State          string         `json:"state"`
	MergeCommitSHA string         `json:"merge_commit_sha"`
	HeadRef        string         `json:"head_ref"`
	BaseRef        string         `json:"base_ref"`
	Stamp          ForgeStamp     `json:"stamp"`
	ClosingIssues  []IssueSummary `json:"closing_issues"`
	DirectUnit     bool           `json:"direct_session_contained_unit"`
}

type PinnedIssue struct {
	IssueSummary
	TitleMatchesWavePattern bool `json:"title_matches_wave_pattern"`
}

type ContainingTracker struct {
	Number  int            `json:"number"`
	Title   string         `json:"title"`
	Body    string         `json:"body"`
	Stamp   ForgeStamp     `json:"stamp"`
	Entries []TrackerEntry `json:"entries"`
	Units   []UnitEvidence `json:"units"`
}

type TrackerEntry struct {
	UnitNumber int    `json:"unit_number"`
	Checked    bool   `json:"checked"`
	Line       string `json:"line"`
}

type UnitEvidence struct {
	Number              int               `json:"number"`
	Title               string            `json:"title"`
	State               string            `json:"state"`
	Body                string            `json:"body"`
	DependenciesSection string            `json:"dependencies_section"`
	Stamp               ForgeStamp        `json:"stamp"`
	ClosingPullRequests []PullRequestRef  `json:"closing_pull_requests"`
	StackedOn           []StackedEvidence `json:"stacked_on"`
}

type PullRequestRef struct {
	Number         int                `json:"number"`
	Title          string             `json:"title"`
	State          string             `json:"state"`
	Merged         bool               `json:"merged"`
	HeadRef        string             `json:"head_ref"`
	BaseRef        string             `json:"base_ref"`
	HeadRepository RepositoryIdentity `json:"head_repository"`
	Stamp          ForgeStamp         `json:"stamp"`
}

type StackedEvidence struct {
	ReferenceNumber   int              `json:"reference_number"`
	ReferenceKind     string           `json:"reference_kind"`
	BasePullRequests  []PullRequestRef `json:"base_pull_requests"`
	ChildPullRequests []PullRequestRef `json:"child_pull_requests"`
}

type RepositoryIdentity struct {
	State         string  `json:"state"`
	NameWithOwner *string `json:"name_with_owner"`
}

type OpenPullRequest struct {
	Number         int                `json:"number"`
	Title          string             `json:"title"`
	HeadRef        string             `json:"head_ref"`
	BaseRef        string             `json:"base_ref"`
	HeadRepository RepositoryIdentity `json:"head_repository"`
	Draft          bool               `json:"draft"`
	Body           string             `json:"body"`
	ScopeLine      string             `json:"scope_line"`
	Stamp          ForgeStamp         `json:"stamp"`
	LinkedIssues   []LinkedIssueScope `json:"linked_issues"`
}

type LinkedIssueScope struct {
	Number        int        `json:"number"`
	Title         string     `json:"title"`
	State         string     `json:"state"`
	Body          string     `json:"body"`
	ScopeSection  string     `json:"scope_section"`
	DeclaredPaths []string   `json:"declared_paths"`
	Stamp         ForgeStamp `json:"stamp"`
}

type MarkerComment struct {
	IssueNumber             int        `json:"issue_number"`
	Kind                    string     `json:"kind"`
	Body                    string     `json:"body"`
	Branch                  string     `json:"branch,omitempty"`
	ReleasesClaimID         int64      `json:"releases_claim_id,omitempty"`
	PlanIssueNumber         int        `json:"plan_issue_number,omitempty"`
	MatchedOpenPullRequests []int      `json:"matched_open_pull_requests"`
	Stamp                   ForgeStamp `json:"stamp"`
	CreatedAt               string     `json:"created_at"`
}

type Ambiguity struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}
