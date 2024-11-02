Accelero is an application that automates Docker deployments seamlessly with zero downtime by leveraging a GitOps strategy. It is a container-based tool that listens for webhooks from your CI pipeline and updates your service with the new image. It is designed to be simple to use and easy to integrate with your existing CI/CD pipeline.

Using Accelero is as simple as pushing your Docker image to your registry. After that, Accelero gets a webhook trigger and takes care of the rest by gracefully updating your service with the new image and shutting down the old one.

```bash
$ docker ps
CONTAINER ID   IMAGE                    COMMAND                  CREATED          STATUS                    PORTS                    NAMES
37ad1f1abe88   arbianshkodra/accelero   "./accelero"             4 seconds ago    Up 4 seconds              0.0.0.0:8000->8000/tcp   accelero
a1edf7ab524e   nginx:1.26.1             "/docker-entrypoint.…"   12 minutes ago   Up 12 minutes (healthy)   80/tcp                   web_0_1724414427072913000
7fe4b38dd103   traefik:v2.9             "/entrypoint.sh --ap…"   12 minutes ago   Up 12 minutes             0.0.0.0:80->80/tcp       traefik_0_1724414406263150000
```

As you can see, Accelero is running and listening on port 8000. You can now push your Docker image to your registry, and Accelero will take care of the rest. To be able to route traffic to your service, you need to configure a reverse proxy like Traefik, but that will be provided by Accelero in the future. On the next section, we will see how to get started with Accelero.

## Sample GitOps Repository

A sample is located within this repo's directory `samples`. Here is a <a href="https://github.com/arbianshkodra/accelero-sample-gitops/">GitOps repository</a> available where you can fork it and use it as a template for your own projects. This repository will contain a `docker-compose.yaml` file and a `sample.env` file with the necessary configuration. You can use this repository to get started with Accelero and automate your deployments with zero downtime.
