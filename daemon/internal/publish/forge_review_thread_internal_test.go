package publish

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/strayrequest"
)

type failingTokenSource struct{ err error }

func (s failingTokenSource) Token(context.Context, string) (InstallationToken, error) {
	return InstallationToken{}, s.err
}

type failingRoundTripper struct{ err error }

func (rt failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, rt.err
}

type graphQLRequestWire struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func reviewThreadsPageResponse(repo string, number int, hasNext bool, cursor string, nodes []map[string]any) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"nameWithOwner": repo,
				"pullRequest": map[string]any{
					"number": number,
					"reviewThreads": map[string]any{
						"pageInfo": map[string]any{
							"hasNextPage": hasNext,
							"endCursor":   cursor,
						},
						"nodes": nodes,
					},
				},
			},
		},
	}
}

func reviewThreadNode(id string, resolved bool, fullDatabaseID string) map[string]any {
	return map[string]any{
		"id":         id,
		"isResolved": resolved,
		"comments": map[string]any{
			"nodes": []map[string]any{{"fullDatabaseId": fullDatabaseID}},
		},
	}
}

func TestResolveReviewThreadSuccessAndPagination(t *testing.T) {
	t.Parallel()
	const root int64 = 4380186748

	t.Run("resolved", func(t *testing.T) {
		queries, mutations := 0, 0
		server := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
				t.Errorf("request = %s %s", r.Method, r.URL.Path)
			}
			var req graphQLRequestWire
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			if strings.Contains(req.Query, "resolveReviewThread") {
				mutations++
				if req.Variables["id"] != "PRRT_target" {
					t.Errorf("mutation id = %v", req.Variables["id"])
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"resolveReviewThread": map[string]any{
							"thread": map[string]any{"id": "PRRT_target", "isResolved": true},
						},
					},
				})
				return
			}
			queries++
			if req.Variables["owner"] != "owner" || req.Variables["name"] != "repo" || req.Variables["number"] != float64(7) || req.Variables["after"] != nil {
				t.Errorf("query variables = %+v", req.Variables)
			}
			_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
				reviewThreadNode("PRRT_other", false, "100"),
				reviewThreadNode("PRRT_target", false, "4380186748"),
			}))
		})))
		defer server.Close()

		f := newForge(draftTestTokenSource{}, server.Client(), server.URL)
		got, err := f.resolveReviewThread(context.Background(), repoRef{"owner", "repo"}, 7, root)
		if err != nil || got != reviewThreadResolved || queries != 1 || mutations != 1 {
			t.Fatalf("got=(%q, %v), queries=%d, mutations=%d", got, err, queries, mutations)
		}
	})

	t.Run("already resolved sends no mutation", func(t *testing.T) {
		queries, mutations := 0, 0
		server := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req graphQLRequestWire
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			if strings.Contains(req.Query, "resolveReviewThread") {
				mutations++
				t.Error("unexpected mutation for already-resolved thread")
				return
			}
			queries++
			_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
				reviewThreadNode("PRRT_target", true, "4380186748"),
			}))
		})))
		defer server.Close()

		f := newForge(draftTestTokenSource{}, server.Client(), server.URL)
		got, err := f.resolveReviewThread(context.Background(), repoRef{"owner", "repo"}, 7, root)
		if err != nil || got != reviewThreadResolved || queries != 1 || mutations != 0 {
			t.Fatalf("got=(%q, %v), queries=%d, mutations=%d", got, err, queries, mutations)
		}
	})

	t.Run("match on second page stops paging", func(t *testing.T) {
		queries, mutations := 0, 0
		server := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req graphQLRequestWire
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			if strings.Contains(req.Query, "resolveReviewThread") {
				mutations++
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"resolveReviewThread": map[string]any{
							"thread": map[string]any{"id": "PRRT_p2", "isResolved": true},
						},
					},
				})
				return
			}
			queries++
			switch queries {
			case 1:
				if req.Variables["after"] != nil {
					t.Errorf("page 1 after = %v", req.Variables["after"])
				}
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, true, "cursor-1", []map[string]any{
					reviewThreadNode("PRRT_p1", false, "100"),
				}))
			case 2:
				if req.Variables["after"] != "cursor-1" {
					t.Errorf("page 2 after = %v", req.Variables["after"])
				}
				// Even though hasNextPage is true, the page carries the match
				// so the loop must stop without requesting page 3.
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, true, "cursor-2", []map[string]any{
					reviewThreadNode("PRRT_p2", false, "4380186748"),
				}))
			default:
				t.Errorf("unexpected extra page query %d", queries)
			}
		})))
		defer server.Close()

		f := newForge(draftTestTokenSource{}, server.Client(), server.URL)
		got, err := f.resolveReviewThread(context.Background(), repoRef{"owner", "repo"}, 7, root)
		if err != nil || got != reviewThreadResolved || queries != 2 || mutations != 1 {
			t.Fatalf("got=(%q, %v), queries=%d, mutations=%d", got, err, queries, mutations)
		}
	})
}

func TestResolveReviewThreadFailures(t *testing.T) {
	t.Parallel()
	const secretBody = "secret-response-body-sentinel"
	const root int64 = 43

	tests := []struct {
		name          string
		wantMutations int
		handle        func(w http.ResponseWriter, queryNum int, isMutation bool)
	}{
		{
			name: "no match",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
					reviewThreadNode("PRRT_other", false, "99"),
				}))
			},
		},
		{
			name: "two matches",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
					reviewThreadNode("PRRT_1", false, "43"),
					reviewThreadNode("PRRT_2", false, "43"),
				}))
			},
		},
		{
			name: "query graphql errors",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errors": []map[string]any{{"message": secretBody}},
				})
			},
		},
		{
			name:          "mutation graphql errors",
			wantMutations: 1,
			handle: func(w http.ResponseWriter, _ int, isMutation bool) {
				if !isMutation {
					_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
						reviewThreadNode("PRRT_1", false, "43"),
					}))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errors": []map[string]any{{"message": secretBody}},
				})
			},
		},
		{
			name: "plain 403",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(secretBody))
			},
		},
		{
			name: "wrong repository",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("other/repo", 7, false, "", []map[string]any{
					reviewThreadNode("PRRT_1", false, "43"),
				}))
			},
		},
		{
			name: "missing repository",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": nil}})
			},
		},
		{
			name: "wrong pull number",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 8, false, "", []map[string]any{
					reviewThreadNode("PRRT_1", false, "43"),
				}))
			},
		},
		{
			name: "missing pull request",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"repository": map[string]any{"nameWithOwner": "owner/repo", "pullRequest": nil}},
				})
			},
		},
		{
			name: "missing thread id",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
					reviewThreadNode("", false, "43"),
				}))
			},
		},
		{
			name: "missing isResolved on query",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
					{
						"id":       "PRRT_1",
						"comments": map[string]any{"nodes": []map[string]any{{"fullDatabaseId": "43"}}},
					},
				}))
			},
		},
		{
			name: "non-canonical fullDatabaseId",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
					reviewThreadNode("PRRT_1", false, "043"),
				}))
			},
		},
		{
			name: "empty next cursor",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, true, "", []map[string]any{
					reviewThreadNode("PRRT_other", false, "99"),
				}))
			},
		},
		{
			name: "repeated next cursor",
			handle: func(w http.ResponseWriter, queryNum int, _ bool) {
				_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, true, "same-cursor", []map[string]any{
					reviewThreadNode("PRRT_other", false, "99"),
				}))
				if queryNum > 2 {
					t.Errorf("repeated cursor looped to query %d", queryNum)
				}
			},
		},
		{
			name:          "wrong thread id on mutation",
			wantMutations: 1,
			handle: func(w http.ResponseWriter, _ int, isMutation bool) {
				if !isMutation {
					_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
						reviewThreadNode("PRRT_1", false, "43"),
					}))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"resolveReviewThread": map[string]any{
							"thread": map[string]any{"id": "PRRT_other", "isResolved": true},
						},
					},
				})
			},
		},
		{
			name:          "missing isResolved on mutation",
			wantMutations: 1,
			handle: func(w http.ResponseWriter, _ int, isMutation bool) {
				if !isMutation {
					_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
						reviewThreadNode("PRRT_1", false, "43"),
					}))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"resolveReviewThread": map[string]any{
							"thread": map[string]any{"id": "PRRT_1"},
						},
					},
				})
			},
		},
		{
			name:          "isResolved false after mutation",
			wantMutations: 1,
			handle: func(w http.ResponseWriter, _ int, isMutation bool) {
				if !isMutation {
					_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
						reviewThreadNode("PRRT_1", false, "43"),
					}))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"resolveReviewThread": map[string]any{
							"thread": map[string]any{"id": "PRRT_1", "isResolved": false},
						},
					},
				})
			},
		},
		{
			name: "malformed json",
			handle: func(w http.ResponseWriter, _ int, _ bool) {
				_, _ = w.Write([]byte("not json: " + secretBody))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queries, mutations := 0, 0
			server := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req graphQLRequestWire
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode: %v", err)
					return
				}
				isMutation := strings.Contains(req.Query, "resolveReviewThread")
				if isMutation {
					mutations++
				} else {
					queries++
				}
				tc.handle(w, queries, isMutation)
			})))
			defer server.Close()

			f := newForge(draftTestTokenSource{}, server.Client(), server.URL)
			got, err := f.resolveReviewThread(context.Background(), repoRef{"owner", "repo"}, 7, root)
			if err != nil || got != reviewThreadFailed {
				t.Fatalf("got=(%q, %v), want (%q, nil)", got, err, reviewThreadFailed)
			}
			if mutations != tc.wantMutations {
				t.Fatalf("mutations = %d, want %d", mutations, tc.wantMutations)
			}
		})
	}
}

func TestResolveReviewThreadRetries(t *testing.T) {
	t.Parallel()

	t.Run("transport error", func(t *testing.T) {
		client := &http.Client{Transport: failingRoundTripper{err: errors.New("dial failed: fixture-token")}}
		f := newForge(draftTestTokenSource{}, client, "https://api.example.test")
		got, err := f.resolveReviewThread(context.Background(), repoRef{"owner", "repo"}, 7, 43)
		if err != nil || got != reviewThreadRetry {
			t.Fatalf("got=(%q, %v), want (%q, nil)", got, err, reviewThreadRetry)
		}
	})

	t.Run("truncated 200 body", func(t *testing.T) {
		server := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer does not implement http.Hijacker")
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"data\":{\"repository\":")
			_ = rw.Flush()
			_ = conn.Close()
		})))
		defer server.Close()

		f := newForge(draftTestTokenSource{}, server.Client(), server.URL)
		got, err := f.resolveReviewThread(context.Background(), repoRef{"owner", "repo"}, 7, 43)
		if err != nil || got != reviewThreadRetry {
			t.Fatalf("got=(%q, %v), want (%q, nil)", got, err, reviewThreadRetry)
		}
	})

	httpCases := []struct {
		name     string
		onMutate bool
		status   int
		headers  map[string]string
		body     string
	}{
		{
			name:   "5xx",
			status: http.StatusBadGateway,
			body:   "upstream secret-body",
		},
		{
			name:   "429",
			status: http.StatusTooManyRequests,
			body:   "rate limited",
		},
		{
			name:    "403 primary rate limit",
			status:  http.StatusForbidden,
			headers: map[string]string{"X-RateLimit-Remaining": "0"},
			body:    "rate limited",
		},
		{
			name:    "403 secondary rate limit",
			status:  http.StatusForbidden,
			headers: map[string]string{"Retry-After": "60"},
			body:    "secondary rate limit",
		},
		{
			name:    "200 query errors with rate limit header",
			status:  http.StatusOK,
			headers: map[string]string{"X-RateLimit-Remaining": "0"},
			body:    `{"errors":[{"message":"API rate limit exceeded"}]}`,
		},
		{
			name:     "200 mutation errors with retry-after header",
			onMutate: true,
			status:   http.StatusOK,
			headers:  map[string]string{"Retry-After": "30"},
			body:     `{"errors":[{"message":"secondary rate limit"}]}`,
		},
	}

	for _, tc := range httpCases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req graphQLRequestWire
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode: %v", err)
					return
				}
				if tc.onMutate && !strings.Contains(req.Query, "resolveReviewThread") {
					_ = json.NewEncoder(w).Encode(reviewThreadsPageResponse("owner/repo", 7, false, "", []map[string]any{
						reviewThreadNode("PRRT_1", false, "43"),
					}))
					return
				}
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})))
			defer server.Close()

			f := newForge(draftTestTokenSource{}, server.Client(), server.URL)
			got, err := f.resolveReviewThread(context.Background(), repoRef{"owner", "repo"}, 7, 43)
			if err != nil || got != reviewThreadRetry {
				t.Fatalf("got=(%q, %v), want (%q, nil)", got, err, reviewThreadRetry)
			}
		})
	}
}

func TestResolveReviewThreadPreSendErrorsRedactSecrets(t *testing.T) {
	t.Parallel()

	t.Run("token mint failure", func(t *testing.T) {
		sent := 0
		server := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			sent++
			w.WriteHeader(http.StatusOK)
		})))
		defer server.Close()

		f := newForge(failingTokenSource{err: errors.New("mint failed: ghs_secret_token")}, server.Client(), server.URL)
		got, err := f.resolveReviewThread(context.Background(), repoRef{"owner", "repo"}, 7, 43)
		if err == nil || got != "" || sent != 0 {
			t.Fatalf("got=(%q, %v), sent=%d", got, err, sent)
		}
		if strings.Contains(err.Error(), "ghs_secret_token") {
			t.Fatalf("token leaked into error: %v", err)
		}
	})

	t.Run("canceled context during transport", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := &http.Client{Transport: failingRoundTripper{err: errors.New("transport: fixture-token")}}
		f := newForge(draftTestTokenSource{}, client, "https://api.example.test")
		got, err := f.resolveReviewThread(ctx, repoRef{"owner", "repo"}, 7, 43)
		if !errors.Is(err, context.Canceled) || got != "" {
			t.Fatalf("got=(%q, %v), want (\"\", context.Canceled)", got, err)
		}
		if strings.Contains(err.Error(), "fixture-token") {
			t.Fatalf("token leaked into error: %v", err)
		}
	})

	t.Run("invalid target", func(t *testing.T) {
		f := newForge(draftTestTokenSource{}, &http.Client{}, "https://api.example.test")
		if got, err := f.resolveReviewThread(context.Background(), repoRef{"owner", "repo"}, 7, 0); err == nil || got != "" {
			t.Fatalf("got=(%q, %v), want error", got, err)
		}
	})
}
