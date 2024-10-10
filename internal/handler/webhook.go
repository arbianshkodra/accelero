package handler

import (
	"encoding/json"
	"net/http"

	"github.com/arbianshkodra/accelero/internal/compose"
	"github.com/arbianshkodra/accelero/internal/git"
	"github.com/sirupsen/logrus"
)

func Webhook(w http.ResponseWriter, r *http.Request) {
	go func() {
		// Define the directory for cloning the repository
		repoDir := "./data"
		// defer os.RemoveAll(repoDir) // Clean up after use if necessary

		if err := git.CloneRepo(repoDir); err != nil {
			logrus.Errorf("Error cloning repository: %v", err)
			return
		}

		if err := compose.RunDockerCompose(repoDir); err != nil {
			logrus.Errorf("Error running docker-compose: %v", err)
			return
		}

		logrus.Info("Webhook processing completed successfully")
	}()

	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "success"}); err != nil {
		logrus.Errorf("Failed to write response: %v", err)
	}
}
