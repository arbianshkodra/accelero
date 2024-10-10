<p style="text-align: center; margin-left: 1.6rem;">
  <img alt="" src="./images/logo.jpg" width="450" />
</p>
<h1 align="center">
  Accelero
</h1>

<p align="center">A container-based tool that automates Docker deployments seamlessly with zero downtime by leveraging <a href="https://codefresh.io/learn/gitops/">GitOps</a> strategy.</p>

## Quick Start

With Accelero, you can automate your new version deployments with zero downtime, by simply pushing your docker image to your registry. After that, Accelero get's a webhook trigger and will take care of the rest, by gracefully updating your service with the new image and shutting down the old one. Run the accelero container with the following command:

```bash
  $ docker run --rm -d \
  --name accelero -p 8000:8000 \
  --env-file path/to/env/file.env \
  -v /var/run/docker.sock:/var/run/docker.sock \
  arbianshkodra/accelero
```
