package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/sirupsen/logrus"
)

func CloneRepo(ctx context.Context, dir string) error {
	repoURL := os.Getenv("REPO_URL")
	username := os.Getenv("REPO_USERNAME")
	token := os.Getenv("REPO_TOKEN")
	branch := os.Getenv("REPO_BRANCH")
	composePath := os.Getenv("COMPOSE_PATH")

	select {
	case <-ctx.Done():
		return fmt.Errorf("clone operation canceled")
	default:
	}

	cloneOptions := &git.CloneOptions{
		URL: repoURL,
		Auth: &http.BasicAuth{
			Username: username,
			Password: token,
		},
		Progress: os.Stdout,
	}

	if branch != "" {
		cloneOptions.ReferenceName = plumbing.NewBranchReferenceName(branch)
	}

	_, err := git.PlainClone(dir, false, cloneOptions)
	if err != nil {
		return fmt.Errorf("failed to clone repository: %w", err)
	}
	logrus.Infof("Cloned repository to %s", dir)

	if err := copyComposeFile(dir, composePath); err != nil {
		return fmt.Errorf("failed to copy docker-compose file: %w", err)
	}

	return nil
}

func copyComposeFile(repoDir, composePath string) error {
	src := filepath.Join(repoDir, composePath)
	dest := filepath.Join(repoDir, "docker-compose.yaml")

	sourceFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer sourceFile.Close()

	destinationFile, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destinationFile.Close()

	if _, err := io.Copy(destinationFile, sourceFile); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	files, err := os.ReadDir(repoDir)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	for _, file := range files {
		if file.Name() != "docker-compose.yaml" {
			if err := os.RemoveAll(filepath.Join(repoDir, file.Name())); err != nil {
				return fmt.Errorf("failed to remove file %s: %w", file.Name(), err)
			}
		}
	}

	logrus.Info("Successfully copied docker-compose file and cleaned up repository directory")
	return nil
}
