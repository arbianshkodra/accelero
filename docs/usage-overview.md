Accelero is itself packaged as a Docker container, so you can run it with a single command. It is as simple as pulling the image `arbianshkodra/accelero`. If you are using the ARM architecture, you can pull the image `arbianshkodra/accelero:arm-<tag>` instead. After that, you can pull the appropriate image from the [arbianshkodra Docker Hub](https://hub.docker.com/r/arbianshkodra/accelero/tags/).

Since Accelero needs to interact with the Docker daemon, you need to mount the Docker socket to the container. This is done by adding the `-v /var/run/docker.sock:/var/run/docker.sock` flag to the `docker run` command. This way, Accelero can listen for webhooks from your CI pipeline and update your service with the new image.

## Environment Variables
Before running the Accelero container, you need to create an environment file with the necessary configuration. The environment file should contain the following variables:

- `REPO_URL` - The URL of your Git repository.
- `REPO_USERNAME` - The username of your Git repository.
- `REPO_TOKEN` - The token of your Git repository.
- `REPO_BRANCH` - The branch of your Git repository.
- `COMPOSE_PATH` - The path to your `docker-compose.yaml` file.
- `SERVICE_NAMES` - The name of the service you want to update.
- `DOCKER_SOCK` - The path to the Docker socket.
- `DOCKER_USERNAME` - Your Docker username.
- `DOCKER_PASSWORD` - Your Docker password.
- `DOCKER_REGISTRY` - Your Docker registry.
- `API_KEY` - Your API key.

A sample can be found in the `sample.env` file in the root of the repository.

Also a GitOps repo will be available where you can fork it and use it as a template for your own projects.

## Running the Accelero Container

Run the Accelero container with the following command:

```bash
  $ docker run --rm -d \
  --name accelero -p 8000:8000 \
  --env-file path/to/env/file.env \
  -v /var/run/docker.sock:/var/run/docker.sock \
  arbianshkodra/accelero
```

## How Accelero Works
Accelero listens for webhooks from your CI pipeline and updates your service with the new image. To get it started up, fork the <a href="https://github.com/arbianshkodra/accelero-sample-gitops/">GitOps repository</a> and adjust the configuration to your needs. It is designed to be simple to use and easy to integrate with your existing CI/CD pipeline. Using Accelero is as simple as pushing your Docker image to your registry. After that, you'll update your `docker-compose.yaml` file with the new image tag and push it to your repository. Then, when the new image tag is pushed, Accelero will get a webhook trigger and take care of the rest by gracefully updating your service with the new image and shutting down the old one.