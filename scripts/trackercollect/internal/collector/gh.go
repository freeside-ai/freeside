package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type QueryRunner interface {
	Query(context.Context, string, map[string]any) ([]byte, error)
}

type GHRunner struct{ host string }

func NewGHRunner(host string) GHRunner { return GHRunner{host: host} }

func (g GHRunner) Query(ctx context.Context, query string, variables map[string]any) ([]byte, error) {
	if !strings.HasPrefix(strings.TrimSpace(query), "query ") {
		return nil, fmt.Errorf("refusing non-query GraphQL document")
	}
	input, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("encode GraphQL input: %w", err)
	}
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "--hostname", g.host, "--input", "-")
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
