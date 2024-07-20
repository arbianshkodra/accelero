package git

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

func CloneRepo(dir string) error {
	repoURL := os.Getenv("REPO_URL")
	username := os.Getenv("REPO_USERNAME")
	token := os.Getenv("REPO_TOKEN")
	branch := os.Getenv("REPO_BRANCH")
	composePath := os.Getenv("COMPOSE_PATH")

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
		log.Printf("Failed to clone repo: %v", err)
		return err
	} else {
		log.Printf("Cloned repo to %s", dir)
	}

	err = copyComposeFile(dir, composePath)
	if err != nil {
		log.Printf("Failed to copy docker-compose file: %v", err)
		return err
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

	_, err = io.Copy(destinationFile, sourceFile)
	if err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	files, err := os.ReadDir(repoDir)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	for _, file := range files {
		if file.Name() != "docker-compose.yaml" {
			err = os.RemoveAll(filepath.Join(repoDir, file.Name()))
			if err != nil {
				return fmt.Errorf("failed to remove file: %w", err)
			}
		}
	}

	return nil
}
