package handler

import (
	"accelero/internal/compose"
	"accelero/internal/git"
	"encoding/json"
	"log"
	"net/http"
)

func Webhook(w http.ResponseWriter, r *http.Request) {
	go func() {
		// Define the directory for cloning the repository
		repoDir := "./data"
		// defer os.RemoveAll(repoDir) // Clean up after use if necessary

		if err := git.CloneRepo(repoDir); err != nil {
			log.Printf("Error cloning repository: %v", err)
			return
		}

		if err := compose.RunDockerCompose(repoDir); err != nil {
			log.Printf("Error running docker-compose: %v", err)
			return
		}
	}()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}
