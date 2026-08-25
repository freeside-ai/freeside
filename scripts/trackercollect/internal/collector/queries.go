package collector

const mergedPullRequestQuery = `query MergedPullRequest($owner: String!, $name: String!, $number: Int!, $pageSize: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    defaultBranchRef { name target { ... on Commit { oid } } }
    pullRequest(number: $number) {
      id databaseId number title state merged updatedAt headRefName baseRefName body
      mergeCommit { oid }
      closingIssuesReferences(first: $pageSize, after: $cursor) {
        nodes {
          id databaseId number title state updatedAt body
          timelineItems(last: 1, itemTypes: [CLOSED_EVENT]) {
            nodes {
              __typename
              ... on ClosedEvent {
                id
                closer {
                  __typename
                  ... on Commit { oid }
                  ... on PullRequest { number merged mergeCommit { oid } }
                }
              }
            }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const pinnedIssuesQuery = `query PinnedIssues($owner: String!, $name: String!, $pageSize: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pinnedIssues(first: $pageSize, after: $cursor) {
      nodes { issue { id databaseId number title state updatedAt body } }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

const openIssuesQuery = `query OpenIssues($owner: String!, $name: String!, $pageSize: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    issues(first: $pageSize, after: $cursor, states: OPEN, orderBy: {field: CREATED_AT, direction: ASC}) {
      nodes { id databaseId number title state updatedAt body }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

const openPullRequestsQuery = `query OpenPullRequests($owner: String!, $name: String!, $pageSize: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: $pageSize, after: $cursor, states: OPEN, orderBy: {field: CREATED_AT, direction: ASC}) {
      nodes {
        id databaseId number title state merged updatedAt headRefName baseRefName isDraft body
        headRepository { nameWithOwner }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

const pullRequestClosingIssuesQuery = `query PullRequestClosingIssues($owner: String!, $name: String!, $number: Int!, $pageSize: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
	  id databaseId number updatedAt
      closingIssuesReferences(first: $pageSize, after: $cursor) {
        nodes { id databaseId number title state updatedAt body }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const issueDetailsQuery = `query IssueDetails($owner: String!, $name: String!, $number: Int!, $pageSize: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      id databaseId number title state updatedAt body
      closedByPullRequestsReferences(first: $pageSize, after: $cursor) {
        nodes {
          id databaseId number title state merged updatedAt headRefName baseRefName body
          headRepository { nameWithOwner }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const issueCommentsQuery = `query IssueComments($owner: String!, $name: String!, $number: Int!, $pageSize: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
	  id databaseId number updatedAt
      comments(first: $pageSize, after: $cursor) {
        nodes { id databaseId body createdAt updatedAt }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const pullRequestReferenceQuery = `query PullRequestReference($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      id databaseId number title state merged updatedAt headRefName baseRefName body
      headRepository { nameWithOwner }
    }
  }
}`

var allQueryDocuments = []string{
	mergedPullRequestQuery,
	pinnedIssuesQuery,
	openIssuesQuery,
	openPullRequestsQuery,
	pullRequestClosingIssuesQuery,
	issueDetailsQuery,
	issueCommentsQuery,
	pullRequestReferenceQuery,
}
