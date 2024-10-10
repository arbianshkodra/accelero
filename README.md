<div align="center">
  <img src="./docs/images/logo.jpg" width="450" />
  
  # Accelero
  Rollout Docker deployments without downtime, using GitOps strategy.
</div>

## Quick Start
With Accelero, you can automate your new version deployments with zero downtime, by simply pushing your docker image to your registry. After that, Accelero get's a webhook trigger and will take care of the rest, by gracefully updating your service with the new image and shutting down the old one. Run the accelero container with the following command:

```bash
  $ docker run --rm -d \
  --name accelero -p 8000:8000 \
  --env-file path/to/env/file.env \
  -v /var/run/docker.sock:/var/run/docker.sock \
  arbianshkodra/accelero
```

Accelero is a container-based tool that automates Docker deployments seamlessly with zero downtime by leveraging [GitOps](https://codefresh.io/learn/gitops/) strategy.

While it seems like a simple tool, it is a powerful one. It is built with the idea of making deployments as easy as possible, by abstracting the complexity of orchestrating a deployment, and making it as simple as pushing a new image to your registry. As of now, it is **not** recommended to use it in production, as it is still in development.

## Documentation
The full documentation can be found at https://docs.accelero.sh.
