When starting Accelero, you need to provide the following environment variables:

- `REPO_URL` - The URL of your Git repository.
- `REPO_USERNAME` - The username of your Git repository.
- `REPO_TOKEN` - The token of your Git repository.
- `REPO_BRANCH` - The branch of your Git repository.
- `COMPOSE_PATH` - The path to your `docker-compose.yaml` file.

These variables are used to fetch the latest changes from your repository and update your service with the new image. These are mandatory variables and need to be provided when starting the Accelero container.

Additionally, you need to provide the following environment variables:

- `SERVICE_NAMES` - The name of the service/s you want to update. Multiple services can be separated by a comma.
- `DOCKER_SOCK` - The path to the Docker socket.
- `DOCKER_USERNAME` - Your Docker username. (Optional)
- `DOCKER_PASSWORD` - Your Docker password. (Optional)
- `DOCKER_REGISTRY` - Your Docker registry. (Optional)
- `API_KEY` - Your API key. (Optional)

If `API_KEY` is provided, Accelero will use it to authenticate incoming webhooks. If not provided, Accelero will not require authentication.

A sample environment file can be found in the `sample.env` file in the root of the repository.
