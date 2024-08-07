package git

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	gg "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-logr/logr"
)

func Clone(url, path string, auth transport.AuthMethod) error {
	_, err := gg.PlainClone(path, false, &gg.CloneOptions{
		Auth: auth,
		URL:  url,
	})

	return err
}

func Pull(path, branch string) error {
	r, err := gg.PlainOpen(path)
	if err != nil {
		return err
	}

	w, err := r.Worktree()
	if err != nil {
		return err
	}

	if err := w.Pull(&gg.PullOptions{
		RemoteName:    "origin",
		ReferenceName: plumbing.NewBranchReferenceName(branch),
	}); err != nil && err != gg.NoErrAlreadyUpToDate {
		return err
	}

	return nil
}

func Checkout(path, branchName string, logger logr.Logger) error {
	r, err := gg.PlainOpen(path)
	if err != nil {
		return err
	}

	w, err := r.Worktree()
	if err != nil {
		return err
	}

	branchRefName := plumbing.NewBranchReferenceName(branchName)
	branchCoOpts := gg.CheckoutOptions{
		Branch: plumbing.ReferenceName(branchRefName),
		Force:  true,
		Create: true,
	}

	if err := w.Checkout(&branchCoOpts); err != nil {
		logger.Error(err, "local checkout of branch failed, will attempt to fetch remote branch of same name.", "branchName", branchName)

		mirrorRemoteBranchRefSpec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branchName, branchName)
		if err := fetchOrigin(r, mirrorRemoteBranchRefSpec); err != nil {
			return err
		}

		return w.Checkout(&branchCoOpts)
	}
	return nil
}

func CommitChange(path, subPath, userInfo, fieldManger string, data []byte, logger logr.Logger) error {
	r, err := gg.PlainOpen(path)
	if err != nil {
		return fmt.Errorf("failed to open repository, path: %s, err: %s", path, err)
	}

	wtree, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to create work tree: %s, err: %s", path, err)
	}

	targetFile := filepath.Join(path, subPath)

	if err := os.MkdirAll(filepath.Dir(targetFile), os.ModePerm); err != nil {
		return fmt.Errorf("failed to make directory, path: %s, err: %s", filepath.Dir(targetFile), err)
	}

	if err := os.WriteFile(targetFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write changes, path: %s, err: %s", targetFile, err)
	}

	if _, err = wtree.Add(subPath); err != nil {
		return fmt.Errorf("failed to add changes, path: %s, err: %s", subPath, err)
	}

	logger.V(1).Info("git add successfully", "file", targetFile)

	commit, err := wtree.Commit(fmt.Sprintf("changed by %s, field manager: %s", userInfo, fieldManger), &gg.CommitOptions{
		Author: &object.Signature{
			Name: userInfo,
			When: time.Now(),
		},
	})
	if err != nil {
		return err
	}

	_, err = r.CommitObject(commit)
	if err != nil {
		return err
	}

	return nil
}

func PushToRemote(path string, auth transport.AuthMethod) error {
	r, err := gg.PlainOpen(path)
	if err != nil {
		return err
	}

	return r.Push(&gg.PushOptions{
		Auth: auth,
	})
}

func fetchOrigin(repo *gg.Repository, refSpecStr string) error {
	remote, err := repo.Remote("origin")
	if err != nil {
		return err
	}

	var refSpecs []config.RefSpec
	if refSpecStr != "" {
		refSpecs = []config.RefSpec{config.RefSpec(refSpecStr)}
	}

	if err = remote.Fetch(&gg.FetchOptions{
		RefSpecs: refSpecs,
	}); err != nil {
		if err == gg.NoErrAlreadyUpToDate {
			fmt.Print("refs already up to date")
		} else {
			return fmt.Errorf("fetch origin failed: %v", err)
		}
	}

	return nil
}
