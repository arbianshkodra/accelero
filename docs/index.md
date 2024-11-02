<p style="text-align: center;">
  <img alt="Accelero Logo" src="./images/logo.jpg" width="450" />
</p>
<h1 align="center">
  Accelero
</h1>

<p align="center">A container-based tool that automates Docker deployments seamlessly with zero downtime by leveraging the <a href="https://codefresh.io/learn/gitops/">GitOps</a> strategy.</p>

## Quick Start

With Accelero, you can automate your deployments with zero downtime by simply pushing your Docker image to your registry. Accelero listens for webhooks and takes care of the rest, gracefully updating your service with the new image and shutting down the old one.

Run the Accelero container with the following command:

```bash
$ docker run --rm -d \
  --name accelero -p 8000:8000 \
  --env-file path/to/env/file.env \
  -v /var/run/docker.sock:/var/run/docker.sock \
  arbianshkodra/accelero
```

Replace `path/to/env/file.env` with the path to your environment variables file. Accelero will start and listen for incoming webhooks on port 8000.

For detailed instructions, see the [Introduction](./docs/introduction.md) and [Usage Overview](./docs/usage-overview.md) sections.