package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPageSize = 100
	defaultPageCap  = 100
	// MaxGraphQLInt is the largest value accepted by GitHub's GraphQL Int scalar.
	MaxGraphQLInt = 1<<31 - 1
)

type pageInfo struct {
	HasNextPage *bool           `json:"hasNextPage"`
	EndCursor   json.RawMessage `json:"endCursor"`
}

type graphConnection[T any] struct {
	Nodes    *[]T      `json:"nodes"`
	PageInfo *pageInfo `json:"pageInfo"`
}

type graphParentIdentity struct {
	ID         string `json:"id"`
	DatabaseID int64  `json:"databaseId"`
	Number     int    `json:"number"`
	UpdatedAt  string `json:"updatedAt"`
}

type graphIssue struct {
	ID         string `json:"id"`
	DatabaseID int64  `json:"databaseId"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
	UpdatedAt  string `json:"updatedAt"`
	Body       string `json:"body"`
	Timeline   *struct {
		Nodes *[]graphClosedEvent `json:"nodes"`
	} `json:"timelineItems"`
	titlePresent bool
	statePresent bool
	bodyPresent  bool
}

type graphClosedEvent struct {
	TypeName string          `json:"__typename"`
	ID       string          `json:"id"`
	Closer   json.RawMessage `json:"closer"`
}

type graphCloser struct {
	TypeName    string `json:"__typename"`
	OID         string `json:"oid"`
	Number      int    `json:"number"`
	Merged      bool   `json:"merged"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	oidPresent    bool
	numberPresent bool
	mergedPresent bool
}

type graphPullRequest struct {
	ID             string          `json:"id"`
	DatabaseID     int64           `json:"databaseId"`
	Number         int             `json:"number"`
	Title          string          `json:"title"`
	State          string          `json:"state"`
	Merged         bool            `json:"merged"`
	UpdatedAt      string          `json:"updatedAt"`
	HeadRefName    string          `json:"headRefName"`
	BaseRefName    string          `json:"baseRefName"`
	IsDraft        bool            `json:"isDraft"`
	Body           string          `json:"body"`
	HeadRepository json.RawMessage `json:"headRepository"`
	MergeCommit    *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	titlePresent  bool
	statePresent  bool
	mergedPresent bool
	draftPresent  bool
	bodyPresent   bool
	headPresent   bool
	basePresent   bool
}

type graphComment struct {
	ID          string `json:"id"`
	DatabaseID  int64  `json:"databaseId"`
	Body        string `json:"body"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	bodyPresent bool
}

func (issue *graphIssue) UnmarshalJSON(data []byte) error {
	type alias graphIssue
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*issue = graphIssue(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	issue.titlePresent = validStringField(fields, "title")
	issue.statePresent = validStringField(fields, "state")
	issue.bodyPresent = validStringField(fields, "body")
	return nil
}

func (pr *graphPullRequest) UnmarshalJSON(data []byte) error {
	type alias graphPullRequest
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*pr = graphPullRequest(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	pr.titlePresent = validStringField(fields, "title")
	pr.statePresent = validStringField(fields, "state")
	pr.mergedPresent = validBoolField(fields, "merged")
	pr.draftPresent = validBoolField(fields, "isDraft")
	pr.bodyPresent = validStringField(fields, "body")
	pr.headPresent = validStringField(fields, "headRefName")
	pr.basePresent = validStringField(fields, "baseRefName")
	return nil
}

func (comment *graphComment) UnmarshalJSON(data []byte) error {
	type alias graphComment
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*comment = graphComment(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	comment.bodyPresent = validStringField(fields, "body")
	return nil
}

func (closer *graphCloser) UnmarshalJSON(data []byte) error {
	type alias graphCloser
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*closer = graphCloser(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	closer.oidPresent = validStringField(fields, "oid")
	closer.numberPresent = validIntField(fields, "number")
	closer.mergedPresent = validBoolField(fields, "merged")
	return nil
}

func validStringField(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func validBoolField(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return false
	}
	var value bool
	return json.Unmarshal(raw, &value) == nil
}

func validIntField(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return false
	}
	var value int
	return json.Unmarshal(raw, &value) == nil
}

type graphPinnedIssue struct {
	Issue graphIssue `json:"issue"`
}

type collector struct {
	config                      Config
	runner                      QueryRunner
	ambiguities                 []Ambiguity
	attributedClosingIssueCount int
	issueCache                  map[int]UnitEvidence
	prCache                     map[int]PullRequestRef
}

func Run(ctx context.Context, config Config, runner QueryRunner, clock func() time.Time) (int, error) {
	snapshot, err := Collect(ctx, config, runner, clock)
	if err != nil {
		return 1, err
	}
	if err := WriteArtifacts(config.OutputDir, snapshot); err != nil {
		return 1, err
	}
	if len(snapshot.Ambiguities) > 0 {
		return 2, nil
	}
	return 0, nil
}

func Collect(ctx context.Context, config Config, runner QueryRunner, clock func() time.Time) (Snapshot, error) {
	if config.PullRequest <= 0 || config.PullRequest > MaxGraphQLInt {
		return Snapshot{}, fmt.Errorf("pull request number must be between 1 and %d", MaxGraphQLInt)
	}
	if config.PageSize <= 0 {
		config.PageSize = defaultPageSize
	}
	if config.PageCap <= 0 {
		config.PageCap = defaultPageCap
	}
	c := &collector{
		config:     config,
		runner:     runner,
		issueCache: make(map[int]UnitEvidence),
		prCache:    make(map[int]PullRequestRef),
	}
	startedAt := clock().UTC()

	merged, headSHA, err := c.fetchMergedPullRequest(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	closingSubject := fmt.Sprintf("pull request #%d closing issues", config.PullRequest)
	closingIssuesComplete := !c.hasAmbiguity("page-cap-truncation", closingSubject)
	if config.Direct && c.attributedClosingIssueCount > 0 {
		return Snapshot{}, fmt.Errorf("pull request #%d has attributed closing issues and cannot be asserted as direct", config.PullRequest)
	}
	if len(merged.ClosingIssues) == 0 && closingIssuesComplete {
		if config.Direct {
			merged.DirectUnit = true
		} else if c.attributedClosingIssueCount == 0 {
			c.ambiguous("unit-origin", fmt.Sprintf("pull request #%d", config.PullRequest), "no attributed closing issue; prompt-backed direct-unit provenance requires --direct")
		}
	}
	pinned, err := c.fetchPinnedIssues(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	openIssues, err := c.fetchOpenIssues(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	openPullRequests, err := c.fetchOpenPullRequests(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	for i := range openPullRequests {
		linked, err := c.fetchPullRequestClosingIssues(ctx, openPullRequests[i].Number)
		if err != nil {
			return Snapshot{}, err
		}
		openPullRequests[i].LinkedIssues = linked
	}

	trackers, commentIssueNumbers, err := c.buildContainingTrackers(ctx, openIssues, merged.ClosingIssues)
	if err != nil {
		return Snapshot{}, err
	}
	for _, issue := range merged.ClosingIssues {
		commentIssueNumbers[issue.Number] = true
	}
	comments, err := c.fetchMarkerComments(ctx, commentIssueNumbers, openPullRequests)
	if err != nil {
		return Snapshot{}, err
	}

	waveTitleMatches := 0
	openWaveMatches := 0
	for _, issue := range pinned {
		if issue.TitleMatchesWavePattern {
			waveTitleMatches++
			if issue.State == "OPEN" {
				openWaveMatches++
			}
		}
	}
	if waveTitleMatches != 1 {
		c.ambiguous("wave-title-count", "pinned issues", fmt.Sprintf("found %d issues matching the canonical wave-tracker title", waveTitleMatches))
	}

	sort.Slice(c.ambiguities, func(i, j int) bool {
		if c.ambiguities[i].Code != c.ambiguities[j].Code {
			return c.ambiguities[i].Code < c.ambiguities[j].Code
		}
		if c.ambiguities[i].Subject != c.ambiguities[j].Subject {
			return c.ambiguities[i].Subject < c.ambiguities[j].Subject
		}
		return c.ambiguities[i].Detail < c.ambiguities[j].Detail
	})

	snapshot := Snapshot{
		SchemaVersion:           SchemaVersion,
		Repository:              config.Repository.String(),
		Collection:              CollectionStamp{DefaultBranchHeadSHA: headSHA, StartedAt: startedAt, CompletedAt: clock().UTC()},
		MergedPullRequest:       merged,
		PinnedIssues:            pinned,
		OpenWaveTitleMatchCount: openWaveMatches,
		OpenIssueInventory:      buildOpenIssueInventory(openIssues),
		ContainingTrackers:      trackers,
		OpenPullRequests:        openPullRequests,
		MarkerComments:          comments,
		Ambiguities:             c.ambiguities,
	}
	normalizeSnapshot(&snapshot)
	return snapshot, nil
}

func normalizeSnapshot(snapshot *Snapshot) {
	if snapshot.MergedPullRequest.ClosingIssues == nil {
		snapshot.MergedPullRequest.ClosingIssues = []IssueSummary{}
	}
	if snapshot.PinnedIssues == nil {
		snapshot.PinnedIssues = []PinnedIssue{}
	}
	if snapshot.ContainingTrackers == nil {
		snapshot.ContainingTrackers = []ContainingTracker{}
	}
	if snapshot.OpenIssueInventory == nil {
		snapshot.OpenIssueInventory = []IssueIdentity{}
	}
	if snapshot.OpenPullRequests == nil {
		snapshot.OpenPullRequests = []OpenPullRequest{}
	}
	if snapshot.MarkerComments == nil {
		snapshot.MarkerComments = []MarkerComment{}
	}
	if snapshot.Ambiguities == nil {
		snapshot.Ambiguities = []Ambiguity{}
	}
	for i := range snapshot.ContainingTrackers {
		tracker := &snapshot.ContainingTrackers[i]
		if tracker.Entries == nil {
			tracker.Entries = []TrackerEntry{}
		}
		if tracker.Units == nil {
			tracker.Units = []UnitEvidence{}
		}
		for j := range tracker.Units {
			unit := &tracker.Units[j]
			if unit.ClosingPullRequests == nil {
				unit.ClosingPullRequests = []PullRequestRef{}
			}
			if unit.StackedOn == nil {
				unit.StackedOn = []StackedEvidence{}
			}
		}
	}
	for i := range snapshot.OpenPullRequests {
		pr := &snapshot.OpenPullRequests[i]
		if pr.LinkedIssues == nil {
			pr.LinkedIssues = []LinkedIssueScope{}
		}
		for j := range pr.LinkedIssues {
			if pr.LinkedIssues[j].DeclaredPaths == nil {
				pr.LinkedIssues[j].DeclaredPaths = []string{}
			}
		}
	}
	for i := range snapshot.MarkerComments {
		if snapshot.MarkerComments[i].MatchedOpenPullRequests == nil {
			snapshot.MarkerComments[i].MatchedOpenPullRequests = []int{}
		}
	}
}

func buildOpenIssueInventory(issues []graphIssue) []IssueIdentity {
	result := make([]IssueIdentity, 0, len(issues))
	for _, issue := range issues {
		result = append(result, IssueIdentity{Number: issue.Number, Stamp: issueStamp(issue)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result
}

func (c *collector) baseVariables() map[string]any {
	return map[string]any{
		"owner":    c.config.Repository.Owner,
		"name":     c.config.Repository.Name,
		"pageSize": c.config.PageSize,
	}
}

func (c *collector) ambiguous(code, subject, detail string) {
	c.ambiguities = append(c.ambiguities, Ambiguity{Code: code, Subject: subject, Detail: detail})
}

func (c *collector) hasAmbiguity(code, subject string) bool {
	for _, ambiguity := range c.ambiguities {
		if ambiguity.Code == code && ambiguity.Subject == subject {
			return true
		}
	}
	return false
}

func decodeResponse(raw []byte, destination any) error {
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode GraphQL response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, item := range envelope.Errors {
			messages = append(messages, item.Message)
		}
		return fmt.Errorf("GraphQL error: %s", strings.Join(messages, "; "))
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("GraphQL response omitted data")
	}
	if err := json.Unmarshal(envelope.Data, destination); err != nil {
		return fmt.Errorf("decode GraphQL data: %w", err)
	}
	return nil
}

func requireConnection[T any](subject string, connection *graphConnection[T]) ([]T, *pageInfo, error) {
	if connection == nil || connection.Nodes == nil || connection.PageInfo == nil {
		return nil, nil, fmt.Errorf("%s connection, nodes, or pageInfo is absent", subject)
	}
	return *connection.Nodes, connection.PageInfo, nil
}

func (c *collector) nextCursor(subject string, page int, info *pageInfo) (*string, bool, error) {
	if info == nil || info.HasNextPage == nil || len(info.EndCursor) == 0 {
		return nil, false, fmt.Errorf("%s pageInfo is missing hasNextPage or endCursor", subject)
	}
	if !*info.HasNextPage {
		if string(info.EndCursor) != "null" {
			var endCursor string
			if err := json.Unmarshal(info.EndCursor, &endCursor); err != nil {
				return nil, false, fmt.Errorf("%s pageInfo has malformed endCursor", subject)
			}
		}
		return nil, false, nil
	}
	var endCursor string
	if err := json.Unmarshal(info.EndCursor, &endCursor); err != nil || endCursor == "" {
		return nil, false, fmt.Errorf("%s pagination hasNextPage without endCursor", subject)
	}
	if page >= c.config.PageCap {
		c.ambiguous("page-cap-truncation", subject, fmt.Sprintf("stopped after %d pages with more data available", page))
		return nil, false, nil
	}
	return &endCursor, true, nil
}

func issueStamp(issue graphIssue) ForgeStamp {
	return ForgeStamp{NodeID: issue.ID, DatabaseID: issue.DatabaseID, UpdatedAt: issue.UpdatedAt, BodySHA256: bodyHash(issue.Body)}
}

func issueSummary(issue graphIssue) IssueSummary {
	return IssueSummary{Number: issue.Number, Title: issue.Title, State: issue.State, Stamp: issueStamp(issue)}
}

func prStamp(pr graphPullRequest) ForgeStamp {
	return ForgeStamp{NodeID: pr.ID, DatabaseID: pr.DatabaseID, UpdatedAt: pr.UpdatedAt, BodySHA256: bodyHash(pr.Body)}
}

func validateForgeObject(kind string, id string, databaseID int64, number int, updatedAt string) error {
	if id == "" || databaseID <= 0 || number <= 0 || updatedAt == "" {
		return fmt.Errorf("%s is missing a node ID, database ID, number, or updatedAt stamp", kind)
	}
	if _, err := time.Parse(time.RFC3339, updatedAt); err != nil {
		return fmt.Errorf("%s has malformed updatedAt %q", kind, updatedAt)
	}
	return nil
}

func validateRequestedParent(kind string, parent graphParentIdentity, requested int) error {
	if err := validateForgeObject(kind, parent.ID, parent.DatabaseID, parent.Number, parent.UpdatedAt); err != nil {
		return err
	}
	if parent.Number != requested {
		return fmt.Errorf("%s response names #%d, want #%d", kind, parent.Number, requested)
	}
	return nil
}

func validateIssue(issue graphIssue) error {
	if err := validateForgeObject(fmt.Sprintf("issue #%d", issue.Number), issue.ID, issue.DatabaseID, issue.Number, issue.UpdatedAt); err != nil {
		return err
	}
	if issue.State != "OPEN" && issue.State != "CLOSED" {
		return fmt.Errorf("issue #%d has invalid state %q", issue.Number, issue.State)
	}
	if !issue.titlePresent || !issue.statePresent || !issue.bodyPresent {
		return fmt.Errorf("issue #%d is missing title, state, or body", issue.Number)
	}
	return nil
}

func validatePullRequest(pr graphPullRequest, requireDraft bool) error {
	if err := validateForgeObject(fmt.Sprintf("pull request #%d", pr.Number), pr.ID, pr.DatabaseID, pr.Number, pr.UpdatedAt); err != nil {
		return err
	}
	if pr.State != "OPEN" && pr.State != "CLOSED" && pr.State != "MERGED" {
		return fmt.Errorf("pull request #%d has invalid state %q", pr.Number, pr.State)
	}
	if pr.HeadRefName == "" || pr.BaseRefName == "" {
		return fmt.Errorf("pull request #%d is missing a head or base ref", pr.Number)
	}
	if !pr.titlePresent || !pr.statePresent || !pr.mergedPresent || !pr.bodyPresent || !pr.headPresent || !pr.basePresent {
		return fmt.Errorf("pull request #%d scalar presence: title=%t state=%t merged=%t body=%t headRef=%t baseRef=%t", pr.Number, pr.titlePresent, pr.statePresent, pr.mergedPresent, pr.bodyPresent, pr.headPresent, pr.basePresent)
	}
	if requireDraft && !pr.draftPresent {
		return fmt.Errorf("open pull request #%d is missing isDraft", pr.Number)
	}
	return nil
}

func (c *collector) fetchMergedPullRequest(ctx context.Context) (MergedPullRequest, string, error) {
	vars := c.baseVariables()
	vars["number"] = c.config.PullRequest
	var cursor *string
	var result MergedPullRequest
	var defaultHead string
	closingSubject := fmt.Sprintf("pull request #%d closing issues", c.config.PullRequest)
	for page := 1; ; page++ {
		vars["cursor"] = cursor
		raw, err := c.runner.Query(ctx, mergedPullRequestQuery, vars)
		if err != nil {
			return result, "", fmt.Errorf("collect merged pull request: %w", err)
		}
		var data struct {
			Repository *struct {
				DefaultBranchRef *struct {
					Target *struct {
						OID string `json:"oid"`
					} `json:"target"`
				} `json:"defaultBranchRef"`
				PullRequest json.RawMessage `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := decodeResponse(raw, &data); err != nil {
			return result, "", err
		}
		if data.Repository == nil || len(data.Repository.PullRequest) == 0 || string(data.Repository.PullRequest) == "null" {
			return result, "", fmt.Errorf("pull request #%d not found", c.config.PullRequest)
		}
		var pr graphPullRequest
		var connections struct {
			Closing *graphConnection[graphIssue] `json:"closingIssuesReferences"`
		}
		if err := json.Unmarshal(data.Repository.PullRequest, &pr); err != nil {
			return result, "", fmt.Errorf("decode pull request #%d: %w", c.config.PullRequest, err)
		}
		if err := json.Unmarshal(data.Repository.PullRequest, &connections); err != nil {
			return result, "", fmt.Errorf("decode pull request #%d connections: %w", c.config.PullRequest, err)
		}
		if err := validatePullRequest(pr, false); err != nil {
			return result, "", err
		}
		if pr.Number != c.config.PullRequest {
			return result, "", fmt.Errorf("pull request response names #%d, want #%d", pr.Number, c.config.PullRequest)
		}
		if page == 1 {
			if !pr.Merged || pr.State != "MERGED" || pr.MergeCommit == nil || pr.MergeCommit.OID == "" {
				return result, "", fmt.Errorf("pull request #%d is not merged", c.config.PullRequest)
			}
			if data.Repository.DefaultBranchRef == nil || data.Repository.DefaultBranchRef.Target == nil || data.Repository.DefaultBranchRef.Target.OID == "" {
				return result, "", errors.New("repository default-branch head is unavailable")
			}
			defaultHead = data.Repository.DefaultBranchRef.Target.OID
			result = MergedPullRequest{
				Number: pr.Number, Title: pr.Title, State: pr.State, MergeCommitSHA: pr.MergeCommit.OID,
				HeadRef: pr.HeadRefName, BaseRef: pr.BaseRefName, Stamp: prStamp(pr),
			}
		} else if !sameMergedPullRequestPage(pr, result) {
			return result, "", fmt.Errorf("pull request #%d identity or stamped fields changed across closing-issue pages", c.config.PullRequest)
		}
		nodes, pageInfo, err := requireConnection(closingSubject, connections.Closing)
		if err != nil {
			return result, "", err
		}
		for _, issue := range nodes {
			if err := validateIssue(issue); err != nil {
				return result, "", err
			}
			attributed, err := issueAttributedToPullRequest(issue, result.Number, result.MergeCommitSHA)
			if err != nil {
				return result, "", err
			}
			if attributed {
				c.attributedClosingIssueCount++
				if issue.State == "CLOSED" {
					result.ClosingIssues = append(result.ClosingIssues, issueSummary(issue))
				} else {
					c.ambiguous("closing-issue-state", fmt.Sprintf("issue #%d", issue.Number), "the requested pull request is the latest closer, but the issue is currently open")
				}
			}
		}
		next, more, err := c.nextCursor(closingSubject, page, pageInfo)
		if err != nil {
			return result, "", err
		}
		if !more {
			break
		}
		cursor = next
	}
	sort.Slice(result.ClosingIssues, func(i, j int) bool { return result.ClosingIssues[i].Number < result.ClosingIssues[j].Number })
	return result, defaultHead, nil
}

func sameMergedPullRequestPage(pr graphPullRequest, retained MergedPullRequest) bool {
	return pr.Number == retained.Number &&
		pr.Title == retained.Title &&
		pr.State == retained.State &&
		pr.Merged &&
		pr.MergeCommit != nil &&
		pr.MergeCommit.OID == retained.MergeCommitSHA &&
		pr.HeadRefName == retained.HeadRef &&
		pr.BaseRefName == retained.BaseRef &&
		prStamp(pr) == retained.Stamp
}

func issueAttributedToPullRequest(issue graphIssue, pullRequestNumber int, mergeCommitSHA string) (bool, error) {
	if issue.Timeline == nil || issue.Timeline.Nodes == nil {
		return false, fmt.Errorf("issue #%d closure timeline or nodes is absent", issue.Number)
	}
	events := *issue.Timeline.Nodes
	if len(events) == 0 && issue.State == "OPEN" {
		return false, nil
	}
	if len(events) != 1 {
		return false, fmt.Errorf("issue #%d closure timeline carries %d latest closed events, want 1", issue.Number, len(events))
	}
	event := events[0]
	if event.TypeName != "ClosedEvent" || event.ID == "" {
		return false, fmt.Errorf("issue #%d closure timeline carries an invalid closed event", issue.Number)
	}
	if len(event.Closer) == 0 {
		return false, fmt.Errorf("issue #%d closed event is missing closer", issue.Number)
	}
	if strings.TrimSpace(string(event.Closer)) == "null" {
		return false, nil
	}
	var closer graphCloser
	if err := json.Unmarshal(event.Closer, &closer); err != nil {
		return false, fmt.Errorf("issue #%d closed event closer: %w", issue.Number, err)
	}
	switch closer.TypeName {
	case "Commit":
		if !closer.oidPresent || closer.OID == "" {
			return false, fmt.Errorf("issue #%d commit closer is missing oid", issue.Number)
		}
		return closer.OID == mergeCommitSHA, nil
	case "PullRequest":
		if !closer.numberPresent || closer.Number <= 0 || !closer.mergedPresent || !closer.Merged || closer.MergeCommit == nil || closer.MergeCommit.OID == "" {
			return false, fmt.Errorf("issue #%d pull request closer is incomplete or unmerged", issue.Number)
		}
		return closer.Number == pullRequestNumber && closer.MergeCommit.OID == mergeCommitSHA, nil
	default:
		return false, fmt.Errorf("issue #%d closed event carries unexpected closer type %q", issue.Number, closer.TypeName)
	}
}

func (c *collector) fetchPinnedIssues(ctx context.Context) ([]PinnedIssue, error) {
	vars := c.baseVariables()
	var cursor *string
	var result []PinnedIssue
	for page := 1; ; page++ {
		vars["cursor"] = cursor
		raw, err := c.runner.Query(ctx, pinnedIssuesQuery, vars)
		if err != nil {
			return nil, fmt.Errorf("collect pinned issues: %w", err)
		}
		var data struct {
			Repository *struct {
				Pinned *graphConnection[graphPinnedIssue] `json:"pinnedIssues"`
			} `json:"repository"`
		}
		if err := decodeResponse(raw, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil {
			return nil, errors.New("repository not found while collecting pinned issues")
		}
		nodes, pageInfo, err := requireConnection("pinned issues", data.Repository.Pinned)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			if err := validateIssue(node.Issue); err != nil {
				return nil, err
			}
			result = append(result, PinnedIssue{IssueSummary: issueSummary(node.Issue), TitleMatchesWavePattern: waveTitlePattern.MatchString(node.Issue.Title)})
		}
		next, more, err := c.nextCursor("pinned issues", page, pageInfo)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		cursor = next
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

func (c *collector) fetchOpenIssues(ctx context.Context) ([]graphIssue, error) {
	vars := c.baseVariables()
	var cursor *string
	var result []graphIssue
	for page := 1; ; page++ {
		vars["cursor"] = cursor
		raw, err := c.runner.Query(ctx, openIssuesQuery, vars)
		if err != nil {
			return nil, fmt.Errorf("collect open issues: %w", err)
		}
		var data struct {
			Repository *struct {
				Issues *graphConnection[graphIssue] `json:"issues"`
			} `json:"repository"`
		}
		if err := decodeResponse(raw, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil {
			return nil, errors.New("repository not found while collecting open issues")
		}
		nodes, pageInfo, err := requireConnection("open issues", data.Repository.Issues)
		if err != nil {
			return nil, err
		}
		for _, issue := range nodes {
			if err := validateIssue(issue); err != nil {
				return nil, err
			}
			result = append(result, issue)
		}
		next, more, err := c.nextCursor("open issues", page, pageInfo)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		cursor = next
	}
	return result, nil
}

func (c *collector) fetchOpenPullRequests(ctx context.Context) ([]OpenPullRequest, error) {
	vars := c.baseVariables()
	var cursor *string
	var result []OpenPullRequest
	for page := 1; ; page++ {
		vars["cursor"] = cursor
		raw, err := c.runner.Query(ctx, openPullRequestsQuery, vars)
		if err != nil {
			return nil, fmt.Errorf("collect open pull requests: %w", err)
		}
		var data struct {
			Repository *struct {
				PullRequests *graphConnection[graphPullRequest] `json:"pullRequests"`
			} `json:"repository"`
		}
		if err := decodeResponse(raw, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil {
			return nil, errors.New("repository not found while collecting open pull requests")
		}
		nodes, pageInfo, err := requireConnection("open pull requests", data.Repository.PullRequests)
		if err != nil {
			return nil, err
		}
		for _, pr := range nodes {
			if err := validatePullRequest(pr, true); err != nil {
				return nil, err
			}
			identity, err := parseRepositoryIdentity(pr.HeadRepository)
			if err != nil {
				return nil, fmt.Errorf("open pull request #%d: %w", pr.Number, err)
			}
			scopeLines := extractScopeLines(pr.Body)
			if len(scopeLines) > 1 {
				c.ambiguous("parse-collision", fmt.Sprintf("pull request #%d", pr.Number), fmt.Sprintf("found %d authoritative Scope lines", len(scopeLines)))
			}
			scopeLine := ""
			if len(scopeLines) > 0 {
				scopeLine = scopeLines[0]
			}
			result = append(result, OpenPullRequest{
				Number: pr.Number, Title: pr.Title, HeadRef: pr.HeadRefName, BaseRef: pr.BaseRefName,
				HeadRepository: identity, Draft: pr.IsDraft, Body: pr.Body,
				ScopeLine: scopeLine, Stamp: prStamp(pr),
			})
		}
		next, more, err := c.nextCursor("open pull requests", page, pageInfo)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		cursor = next
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

func (c *collector) fetchPullRequestClosingIssues(ctx context.Context, number int) ([]LinkedIssueScope, error) {
	vars := c.baseVariables()
	vars["number"] = number
	var cursor *string
	var result []LinkedIssueScope
	var retainedParent graphParentIdentity
	for page := 1; ; page++ {
		vars["cursor"] = cursor
		raw, err := c.runner.Query(ctx, pullRequestClosingIssuesQuery, vars)
		if err != nil {
			return nil, fmt.Errorf("collect pull request #%d linked issues: %w", number, err)
		}
		var data struct {
			Repository *struct {
				PullRequest *struct {
					graphParentIdentity
					Closing *graphConnection[graphIssue] `json:"closingIssuesReferences"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := decodeResponse(raw, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil || data.Repository.PullRequest == nil {
			return nil, fmt.Errorf("open pull request #%d disappeared during collection", number)
		}
		currentParent := data.Repository.PullRequest.graphParentIdentity
		if err := validateRequestedParent(fmt.Sprintf("pull request #%d", number), currentParent, number); err != nil {
			return nil, err
		}
		if page == 1 {
			retainedParent = currentParent
		} else if currentParent != retainedParent {
			return nil, fmt.Errorf("pull request #%d identity or updatedAt changed across linked-issue pages", number)
		}
		nodes, pageInfo, err := requireConnection(fmt.Sprintf("pull request #%d linked issues", number), data.Repository.PullRequest.Closing)
		if err != nil {
			return nil, err
		}
		for _, issue := range nodes {
			if err := validateIssue(issue); err != nil {
				return nil, err
			}
			scopeSections := extractSections(issue.Body, "Scope / declared paths")
			if len(scopeSections) > 1 {
				c.ambiguous("parse-collision", fmt.Sprintf("issue #%d", issue.Number), fmt.Sprintf("found %d Scope / declared paths sections", len(scopeSections)))
			}
			scope := ""
			if len(scopeSections) > 0 {
				scope = scopeSections[0]
			}
			result = append(result, LinkedIssueScope{
				Number: issue.Number, Title: issue.Title, State: issue.State, Body: issue.Body,
				ScopeSection: scope, DeclaredPaths: parseDeclaredPaths(scope), Stamp: issueStamp(issue),
			})
		}
		next, more, err := c.nextCursor(fmt.Sprintf("pull request #%d linked issues", number), page, pageInfo)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		cursor = next
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

func (c *collector) buildContainingTrackers(ctx context.Context, openIssues []graphIssue, closing []IssueSummary) ([]ContainingTracker, map[int]bool, error) {
	closingSet := make(map[int]bool, len(closing))
	for _, issue := range closing {
		closingSet[issue.Number] = true
	}
	commentIssues := make(map[int]bool)
	var trackers []ContainingTracker
	for _, issue := range openIssues {
		entries, invalidNumbers := parseCheckboxEntries(issue.Body)
		containsClosing := false
		for _, entry := range entries {
			containsClosing = containsClosing || closingSet[entry.UnitNumber]
		}
		if !containsClosing {
			continue
		}
		if len(invalidNumbers) > 0 {
			c.ambiguous("malformed-tracker-entry", fmt.Sprintf("issue #%d", issue.Number), fmt.Sprintf("invalid issue numbers: %s", strings.Join(invalidNumbers, ", ")))
			continue
		}
		implementationOrder := extractSections(issue.Body, "Implementation order")
		if len(implementationOrder) != 1 {
			c.ambiguous("tracker-structure", fmt.Sprintf("issue #%d", issue.Number), fmt.Sprintf("expected exactly one Implementation order section, found %d", len(implementationOrder)))
			continue
		}
		tracker := ContainingTracker{Number: issue.Number, Title: issue.Title, Body: issue.Body, Stamp: issueStamp(issue), Entries: entries}
		counts := make(map[int]int)
		for _, entry := range entries {
			counts[entry.UnitNumber]++
		}
		unitNumbers := make([]int, 0, len(counts))
		for number, count := range counts {
			unitNumbers = append(unitNumbers, number)
			if count > 1 {
				c.ambiguous("duplicate-tracker-entry", fmt.Sprintf("issue #%d", issue.Number), fmt.Sprintf("unit #%d appears %d times", number, count))
			}
		}
		sort.Ints(unitNumbers)
		for _, number := range unitNumbers {
			unit, err := c.fetchUnit(ctx, number)
			if err != nil {
				return nil, nil, err
			}
			tracker.Units = append(tracker.Units, unit)
			if unit.State == "OPEN" {
				commentIssues[number] = true
			}
		}
		trackers = append(trackers, tracker)
	}
	sort.Slice(trackers, func(i, j int) bool { return trackers[i].Number < trackers[j].Number })
	return trackers, commentIssues, nil
}

func (c *collector) fetchUnit(ctx context.Context, number int) (UnitEvidence, error) {
	if cached, ok := c.issueCache[number]; ok {
		return cached, nil
	}
	issue, pulls, err := c.fetchIssueDetails(ctx, number)
	if err != nil {
		return UnitEvidence{}, err
	}
	dependencySections := extractSections(issue.Body, "Dependencies")
	if len(dependencySections) != 1 {
		c.ambiguous("parse-collision", fmt.Sprintf("issue #%d", number), fmt.Sprintf("expected exactly one Dependencies section, found %d", len(dependencySections)))
	}
	dependencies := ""
	if len(dependencySections) == 1 {
		dependencies = dependencySections[0]
	}
	unit := UnitEvidence{
		Number: issue.Number, Title: issue.Title, State: issue.State, Body: issue.Body,
		DependenciesSection: dependencies, Stamp: issueStamp(issue),
		ClosingPullRequests: pulls,
	}
	c.issueCache[number] = unit
	stackedReferences, invalidNumbers := parseStackedReferences(unit.DependenciesSection)
	if len(invalidNumbers) > 0 {
		c.ambiguous("malformed-stacked-reference", fmt.Sprintf("issue #%d", number), fmt.Sprintf("invalid issue numbers: %s", strings.Join(invalidNumbers, ", ")))
	}
	for _, ref := range stackedReferences {
		stacked := StackedEvidence{ReferenceNumber: ref.Number, ReferenceKind: ref.Kind, ChildPullRequests: pulls}
		if ref.Kind == "pull_request" {
			base, err := c.fetchPullRequestReference(ctx, ref.Number)
			if err != nil {
				return UnitEvidence{}, err
			}
			stacked.BasePullRequests = []PullRequestRef{base}
		} else {
			_, basePulls, err := c.fetchIssueDetails(ctx, ref.Number)
			if err != nil {
				return UnitEvidence{}, fmt.Errorf("stacked-on base issue #%d: %w", ref.Number, err)
			}
			stacked.BasePullRequests = basePulls
		}
		unit.StackedOn = append(unit.StackedOn, stacked)
	}
	c.issueCache[number] = unit
	return unit, nil
}

func (c *collector) fetchIssueDetails(ctx context.Context, number int) (graphIssue, []PullRequestRef, error) {
	vars := c.baseVariables()
	vars["number"] = number
	var cursor *string
	var issue graphIssue
	var pulls []PullRequestRef
	for page := 1; ; page++ {
		vars["cursor"] = cursor
		raw, err := c.runner.Query(ctx, issueDetailsQuery, vars)
		if err != nil {
			return issue, nil, fmt.Errorf("collect issue #%d: %w", number, err)
		}
		var data struct {
			Repository *struct {
				Issue json.RawMessage `json:"issue"`
			} `json:"repository"`
		}
		if err := decodeResponse(raw, &data); err != nil {
			return issue, nil, err
		}
		if data.Repository == nil || len(data.Repository.Issue) == 0 || string(data.Repository.Issue) == "null" {
			return issue, nil, fmt.Errorf("issue #%d not found", number)
		}
		var connections struct {
			Closing *graphConnection[graphPullRequest] `json:"closedByPullRequestsReferences"`
		}
		var currentIssue graphIssue
		if err := json.Unmarshal(data.Repository.Issue, &currentIssue); err != nil {
			return issue, nil, fmt.Errorf("decode issue #%d: %w", number, err)
		}
		if err := json.Unmarshal(data.Repository.Issue, &connections); err != nil {
			return issue, nil, fmt.Errorf("decode issue #%d connections: %w", number, err)
		}
		if err := validateIssue(currentIssue); err != nil {
			return issue, nil, err
		}
		if currentIssue.Number != number {
			return issue, nil, fmt.Errorf("issue response names #%d, want #%d", currentIssue.Number, number)
		}
		if page == 1 {
			issue = currentIssue
		} else if !sameIssuePage(currentIssue, issue) {
			return issue, nil, fmt.Errorf("issue #%d identity or stamped fields changed across closing-pull-request pages", number)
		}
		nodes, pageInfo, err := requireConnection(fmt.Sprintf("issue #%d closing pull requests", number), connections.Closing)
		if err != nil {
			return issue, nil, err
		}
		for _, pr := range nodes {
			if err := validatePullRequest(pr, false); err != nil {
				return issue, nil, err
			}
			converted, err := convertPullRequestRef(pr)
			if err != nil {
				return issue, nil, fmt.Errorf("issue #%d closing pull request #%d: %w", number, pr.Number, err)
			}
			pulls = append(pulls, converted)
			c.prCache[converted.Number] = converted
		}
		next, more, err := c.nextCursor(fmt.Sprintf("issue #%d closing pull requests", number), page, pageInfo)
		if err != nil {
			return issue, nil, err
		}
		if !more {
			break
		}
		cursor = next
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
	return issue, pulls, nil
}

func sameIssuePage(current, retained graphIssue) bool {
	return current.Number == retained.Number &&
		current.Title == retained.Title &&
		current.State == retained.State &&
		issueStamp(current) == issueStamp(retained)
}

func convertPullRequestRef(pr graphPullRequest) (PullRequestRef, error) {
	identity, err := parseRepositoryIdentity(pr.HeadRepository)
	if err != nil {
		return PullRequestRef{}, err
	}
	return PullRequestRef{
		Number: pr.Number, Title: pr.Title, State: pr.State, Merged: pr.Merged,
		HeadRef: pr.HeadRefName, BaseRef: pr.BaseRefName, HeadRepository: identity, Stamp: prStamp(pr),
	}, nil
}

func (c *collector) fetchPullRequestReference(ctx context.Context, number int) (PullRequestRef, error) {
	if cached, ok := c.prCache[number]; ok {
		return cached, nil
	}
	vars := c.baseVariables()
	vars["number"] = number
	raw, err := c.runner.Query(ctx, pullRequestReferenceQuery, vars)
	if err != nil {
		return PullRequestRef{}, fmt.Errorf("collect pull request #%d: %w", number, err)
	}
	var data struct {
		Repository *struct {
			PullRequest *graphPullRequest `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := decodeResponse(raw, &data); err != nil {
		return PullRequestRef{}, err
	}
	if data.Repository == nil || data.Repository.PullRequest == nil {
		return PullRequestRef{}, fmt.Errorf("pull request #%d not found", number)
	}
	if err := validatePullRequest(*data.Repository.PullRequest, false); err != nil {
		return PullRequestRef{}, err
	}
	if data.Repository.PullRequest.Number != number {
		return PullRequestRef{}, fmt.Errorf("pull request response names #%d, want #%d", data.Repository.PullRequest.Number, number)
	}
	result, err := convertPullRequestRef(*data.Repository.PullRequest)
	if err != nil {
		return PullRequestRef{}, fmt.Errorf("pull request #%d: %w", number, err)
	}
	c.prCache[number] = result
	return result, nil
}

func (c *collector) fetchMarkerComments(ctx context.Context, issueNumbers map[int]bool, openPRs []OpenPullRequest) ([]MarkerComment, error) {
	numbers := make([]int, 0, len(issueNumbers))
	for number := range issueNumbers {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	var result []MarkerComment
	for _, number := range numbers {
		comments, err := c.fetchIssueComments(ctx, number)
		if err != nil {
			return nil, err
		}
		for _, comment := range comments {
			marker, keep := c.parseMarkerComment(number, comment, openPRs)
			if keep {
				result = append(result, marker)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt != result[j].CreatedAt {
			return result[i].CreatedAt < result[j].CreatedAt
		}
		return result[i].Stamp.DatabaseID < result[j].Stamp.DatabaseID
	})
	return result, nil
}

func (c *collector) fetchIssueComments(ctx context.Context, number int) ([]graphComment, error) {
	vars := c.baseVariables()
	vars["number"] = number
	var cursor *string
	var result []graphComment
	var retainedParent graphParentIdentity
	for page := 1; ; page++ {
		vars["cursor"] = cursor
		raw, err := c.runner.Query(ctx, issueCommentsQuery, vars)
		if err != nil {
			return nil, fmt.Errorf("collect issue #%d comments: %w", number, err)
		}
		var data struct {
			Repository *struct {
				Issue *struct {
					graphParentIdentity
					Comments *graphConnection[graphComment] `json:"comments"`
				} `json:"issue"`
			} `json:"repository"`
		}
		if err := decodeResponse(raw, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil || data.Repository.Issue == nil {
			return nil, fmt.Errorf("issue #%d disappeared while collecting comments", number)
		}
		currentParent := data.Repository.Issue.graphParentIdentity
		if err := validateRequestedParent(fmt.Sprintf("issue #%d", number), currentParent, number); err != nil {
			return nil, err
		}
		if page == 1 {
			retainedParent = currentParent
		} else if currentParent != retainedParent {
			return nil, fmt.Errorf("issue #%d identity or updatedAt changed across comment pages", number)
		}
		nodes, pageInfo, err := requireConnection(fmt.Sprintf("issue #%d comments", number), data.Repository.Issue.Comments)
		if err != nil {
			return nil, err
		}
		for _, comment := range nodes {
			if err := validateForgeObject(fmt.Sprintf("issue #%d comment", number), comment.ID, comment.DatabaseID, number, comment.UpdatedAt); err != nil {
				return nil, err
			}
			if _, err := time.Parse(time.RFC3339, comment.CreatedAt); err != nil {
				return nil, fmt.Errorf("issue #%d comment %d has malformed createdAt %q", number, comment.DatabaseID, comment.CreatedAt)
			}
			if !comment.bodyPresent {
				return nil, fmt.Errorf("issue #%d comment %d is missing body", number, comment.DatabaseID)
			}
			result = append(result, comment)
		}
		next, more, err := c.nextCursor(fmt.Sprintf("issue #%d comments", number), page, pageInfo)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		cursor = next
	}
	return result, nil
}

func (c *collector) parseMarkerComment(issueNumber int, comment graphComment, openPRs []OpenPullRequest) (MarkerComment, bool) {
	body := comment.Body
	marker := MarkerComment{
		IssueNumber: issueNumber, Body: body, CreatedAt: comment.CreatedAt,
		Stamp: ForgeStamp{NodeID: comment.ID, DatabaseID: comment.DatabaseID, UpdatedAt: comment.UpdatedAt, BodySHA256: bodyHash(body)},
	}
	evidence := markdownEvidence(body)
	claimMarkers := claimMarkerPattern.FindAllStringIndex(evidence, -1)
	releaseMarkers := releaseMarkerPattern.FindAllStringIndex(evidence, -1)
	reservationMarkers := reserveMarkerPattern.FindAllStringIndex(evidence, -1)
	markerCount := len(claimMarkers) + len(releaseMarkers) + len(reservationMarkers)
	if markerCount == 0 {
		return MarkerComment{}, false
	}
	if markerCount != 1 {
		marker.Kind = "mixed-or-duplicate"
		c.ambiguous("malformed-marker-comment", fmt.Sprintf("comment %d", comment.DatabaseID), "comment must have exactly one standalone marker line")
		return marker, true
	}
	switch {
	case len(claimMarkers) == 1:
		marker.Kind = "claim"
		matches := claimPattern.FindAllStringSubmatch(evidence, -1)
		if len(matches) != 1 {
			c.ambiguous("malformed-marker-comment", fmt.Sprintf("comment %d", comment.DatabaseID), "claim marker must have exactly one canonical Claim line")
			return marker, true
		}
		marker.Branch = matches[0][1]
		for _, pr := range openPRs {
			if pr.HeadRef == marker.Branch && identityMatches(pr.HeadRepository, c.config.Repository.NameWithOwner()) {
				marker.MatchedOpenPullRequests = append(marker.MatchedOpenPullRequests, pr.Number)
			}
		}
	case len(releaseMarkers) == 1:
		marker.Kind = "release"
		branches := releasePattern.FindAllStringSubmatch(evidence, -1)
		claimIDs := releasesIDPattern.FindAllStringSubmatch(evidence, -1)
		if len(branches) != 1 || len(claimIDs) != 1 {
			c.ambiguous("malformed-marker-comment", fmt.Sprintf("comment %d", comment.DatabaseID), "release marker must have exactly one Release and Releases-claim line")
			return marker, true
		}
		marker.Branch = branches[0][1]
		claimID, err := strconv.ParseInt(claimIDs[0][1], 10, 64)
		if err != nil || claimID <= 0 {
			c.ambiguous("malformed-marker-comment", fmt.Sprintf("comment %d", comment.DatabaseID), "Releases-claim must be a positive 64-bit integer")
			return marker, true
		}
		marker.ReleasesClaimID = claimID
	case len(reservationMarkers) == 1:
		marker.Kind = "planning-reservation"
		plans := planPattern.FindAllStringSubmatch(evidence, -1)
		if len(plans) != 1 {
			c.ambiguous("malformed-marker-comment", fmt.Sprintf("comment %d", comment.DatabaseID), "planning reservation marker must have exactly one canonical Plan line")
			return marker, true
		}
		parsed, err := strconv.ParseInt(plans[0][1], 10, 32)
		planIssueNumber := int(parsed)
		if err != nil || planIssueNumber <= 0 || planIssueNumber != issueNumber {
			c.ambiguous("malformed-marker-comment", fmt.Sprintf("comment %d", comment.DatabaseID), fmt.Sprintf("Plan must reference enclosing issue #%d with a positive GraphQL Int", issueNumber))
			return marker, true
		}
		marker.PlanIssueNumber = planIssueNumber
	}
	return marker, true
}
