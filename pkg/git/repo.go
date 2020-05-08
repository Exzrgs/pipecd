// Copyright 2020 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package git

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

var (
	ErrNoChange = errors.New("no change")
)

// Repo provides functions to get and handle git data.
type Repo interface {
	GetPath() string
	SetUser(ctx context.Context, username, email string) error
	ListCommits(ctx context.Context, visionRange string) ([]Commit, error)
	GetCommitHashForRev(ctx context.Context, rev string) (string, error)
	Checkout(ctx context.Context, commitish string) error
	CheckoutPullRequest(ctx context.Context, number int, branch string) error
	Clean() error

	Push(ctx context.Context, branch string) error
	CommitChanges(ctx context.Context, branch, message string, newBranch bool, changes map[string][]byte) error
}

type repo struct {
	dir     string
	gitPath string
	remote  string
	logger  *zap.Logger
}

// NewRepo creates a new Repo instance.
func NewRepo(dir, gitPath, remote string, logger *zap.Logger) Repo {
	return &repo{
		dir:     dir,
		gitPath: gitPath,
		remote:  remote,
		logger:  logger.With(zap.String("repo", remote)),
	}
}

// GetPath returns the path to the local git directory.
func (r *repo) GetPath() string {
	return r.dir
}

// SetUser configures username and email for local user of this repo.
func (r *repo) SetUser(ctx context.Context, username, email string) error {
	if out, err := r.runGitCommand(ctx, "config", "user.name", username); err != nil {
		r.logger.Error("failed to config user.name",
			zap.String("out", string(out)),
			zap.Error(err),
		)
		return err
	}
	if out, err := r.runGitCommand(ctx, "config", "user.email", email); err != nil {
		r.logger.Error("failed to config user.email",
			zap.String("out", string(out)),
			zap.Error(err),
		)
		return err
	}
	return nil
}

// ListCommits returns a list of commits in a given revision range.
func (r *repo) ListCommits(ctx context.Context, revisionRange string) ([]Commit, error) {
	args := []string{
		"log",
		"--no-decorate",
		fmt.Sprintf("--pretty=format:%s", commitLogFormat),
	}
	if revisionRange != "" {
		args = append(args, revisionRange)
	}
	out, err := r.runGitCommand(ctx, args...)
	if err != nil {
		r.logger.Error("failed to log commits",
			zap.String("out", string(out)),
			zap.Error(err),
		)
		return nil, err
	}
	return parseCommits(string(out))
}

// GetCommitHashForRev returns the hash value of the commit for a given rev.
func (r *repo) GetCommitHashForRev(ctx context.Context, rev string) (string, error) {
	out, err := r.runGitCommand(ctx, "rev-parse", rev)
	if err != nil {
		r.logger.Error("failed to get commit hash for rev",
			zap.String("rev", rev),
			zap.String("out", string(out)),
			zap.Error(err),
		)
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Checkout checkouts to a given commitish.
func (r *repo) Checkout(ctx context.Context, commitish string) error {
	out, err := r.runGitCommand(ctx, "checkout", commitish)
	if err != nil {
		r.logger.Error("failed to checkout",
			zap.String("out", string(out)),
			zap.String("commitish", commitish),
			zap.Error(err),
		)
		return err
	}
	return nil
}

// CheckoutPullRequest checkouts to the latest commit of a given pull request.
func (r *repo) CheckoutPullRequest(ctx context.Context, number int, branch string) error {
	target := fmt.Sprintf("pull/%d/head:%s", number, branch)
	out, err := r.runGitCommand(ctx, "fetch", r.remote, target)
	if err != nil {
		r.logger.Error("failed to checkout pull request",
			zap.String("out", string(out)),
			zap.Int("number", number),
			zap.Error(err),
		)
		return err
	}
	return r.Checkout(ctx, branch)
}

// Push pushes local changes of a given branch to the remote.
func (r *repo) Push(ctx context.Context, branch string) error {
	out, err := r.runGitCommand(ctx, "push", r.remote, branch)
	if err != nil {
		r.logger.Error("failed to push",
			zap.String("out", string(out)),
			zap.String("branch", branch),
			zap.Error(err),
		)
		return err
	}
	return nil
}

// CommitChanges commits some changes into a branch.
func (r *repo) CommitChanges(ctx context.Context, branch, message string, newBranch bool, changes map[string][]byte) error {
	if newBranch {
		if err := r.checkoutNewBranch(ctx, branch); err != nil {
			return fmt.Errorf("failed to checkout new branch, branch: %v, error: %v", branch, err)
		}
	} else {
		if err := r.Checkout(ctx, branch); err != nil {
			return fmt.Errorf("failed to checkout branch, branch: %v, error: %v", branch, err)
		}
	}
	// Apply the changes.
	for p, bytes := range changes {
		filePath := filepath.Join(r.dir, p)
		dirPath := filepath.Dir(filePath)
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			if err := os.MkdirAll(dirPath, os.ModePerm); err != nil {
				return fmt.Errorf("failed to create directory, dir: %s, err: %v", dirPath, err)
			}
		}
		if err := ioutil.WriteFile(filePath, bytes, os.ModePerm); err != nil {
			return fmt.Errorf("failed to write file, file: %s, error: %v", filePath, err)
		}
	}
	// Commit the changes.
	if err := r.addCommit(ctx, message); err != nil {
		return fmt.Errorf("failed to commit, branch: %s, error: %v", branch, err)
	}
	return nil
}

func (r *repo) checkoutNewBranch(ctx context.Context, branch string) error {
	out, err := r.runGitCommand(ctx, "checkout", "-b", branch)
	if err != nil {
		r.logger.Error("failed to checkout new branch",
			zap.String("out", string(out)),
			zap.String("branch", branch),
			zap.Error(err),
		)
		return err
	}
	return nil
}

func (r repo) addCommit(ctx context.Context, message string) error {
	out, err := r.runGitCommand(ctx, "add", ".")
	if err != nil {
		r.logger.Error("failed to add current directory",
			zap.String("out", string(out)),
			zap.Error(err),
		)
		return err
	}
	out, err = r.runGitCommand(ctx, "commit", "-m", message)
	if err != nil {
		msg := string(out)
		r.logger.Error("failed to commit",
			zap.String("out", msg),
			zap.Error(err),
		)
		if strings.Contains(msg, "nothing to commit, working tree clean") {
			return ErrNoChange
		}
		return err
	}
	return nil
}

// Clean deletes all local git data.
func (r repo) Clean() error {
	return os.RemoveAll(r.dir)
}

func (r *repo) runGitCommand(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.gitPath, args...)
	cmd.Dir = r.dir
	return cmd.CombinedOutput()
}
