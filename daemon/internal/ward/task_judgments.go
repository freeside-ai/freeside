package ward

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

type (
	taskOwnerKey struct{}
	taskOwner    struct {
		Task domain.TaskID `json:"task"`
		Run  domain.RunID  `json:"run"`
	}
)

// WithTaskOwner binds advisory calls to the same task as their engine work.
// It carries coordinates, never authorization; the engine checks the durable
// fence before registration and cancels this context when Stop commits.
func WithTaskOwner(ctx context.Context, task domain.TaskID, run domain.RunID) context.Context {
	return context.WithValue(ctx, taskOwnerKey{}, taskOwner{Task: task, Run: run})
}

type taskJudgmentRecord struct {
	Owner     taskOwner `json:"owner"`
	Site      string    `json:"site"`
	Joined    bool      `json:"joined"`
	Quiescent bool      `json:"quiescent"`
}

// TaskJudgments sits below inference.Client, whose timeout may return while
// Driver.Complete still runs. Each actual provider entry is journaled first.
// Drivers without a concrete quiescence adapter still receive cancellation,
// but their return alone cannot confirm native descendants are absent.
type TaskJudgments struct {
	root   string
	driver inference.Driver
	mu     sync.Mutex
}

func NewTaskJudgments(root string, driver inference.Driver) (*TaskJudgments, error) {
	if !filepath.IsAbs(root) || driver == nil {
		return nil, errors.New("task judgments need an absolute ownership directory and driver")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.Join(err, errors.New("task judgment root must be a private directory"))
	}
	return &TaskJudgments{root: root, driver: driver}, nil
}

func (j *TaskJudgments) Complete(ctx context.Context, req inference.Request, secret inference.Secret) (inference.Response, error) {
	owner, bound := ctx.Value(taskOwnerKey{}).(taskOwner)
	if !bound {
		return j.driver.Complete(ctx, req, secret)
	}
	j.mu.Lock()
	registered := false
	defer func() {
		if !registered {
			j.mu.Unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return inference.Response{}, err
	}
	label, err := newOwnershipLabel()
	if err != nil {
		return inference.Response{}, err
	}
	dir := filepath.Join(j.root, verificationRunKey(owner.Run))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return inference.Response{}, err
	}
	path := filepath.Join(dir, verificationRunKey(domain.RunID(label.Value))+".json")
	record := taskJudgmentRecord{Owner: owner, Site: req.SiteID}
	write := func() error {
		body, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return atomicfile.WriteFile(path, body, 0o600)
	}
	if err := write(); err != nil {
		return inference.Response{}, err
	}
	registered = true
	j.mu.Unlock()
	var response inference.Response
	if proven, ok := j.driver.(interface {
		CompleteAndConfirm(context.Context, inference.Request, inference.Secret) (inference.Response, bool, error)
	}); ok {
		response, record.Quiescent, err = proven.CompleteAndConfirm(ctx, req, secret)
	} else {
		response, err = j.driver.Complete(ctx, req, secret)
	}
	record.Joined = true
	return response, errors.Join(err, write())
}

func (j *TaskJudgments) ConfirmRun(ctx context.Context, task domain.TaskID, run domain.RunID) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(j.root, verificationRunKey(run)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(j.root, verificationRunKey(run), entry.Name()))
		var record taskJudgmentRecord
		if err != nil {
			return err
		}
		if err := json.Unmarshal(body, &record); err != nil {
			return err
		}
		if record.Owner.Task != task || record.Owner.Run != run || record.Site == "" || !record.Joined || !record.Quiescent {
			return errors.New("task judgment has no matching native-process quiescence proof")
		}
	}
	return nil
}
