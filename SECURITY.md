# Security Policy

## Supported Versions

Security updates will be issued only for the latest version of Accelero. The software is set to auto-update by default when using the `latest` tag, ensuring you automatically receive these security updates as they become available.

## Reporting a Vulnerability

If you discover a critical vulnerability that could expose the system to external attacks, please report it directly via email to arbianshkodra@gmail.com. We aim to respond as promptly as possible, but please note that as a community project, we cannot guarantee immediate responses.

For less critical vulnerabilities, please report them through the GitHub issues page.

## Dependency Advisories Not Applicable to Accelero

Some Dependabot advisories on transitive dependencies do not affect Accelero's actual attack surface. These are tracked here rather than acted on because there is no applicable fix:

### Moby / Docker Engine advisories

Accelero imports `github.com/docker/docker` **only as a client library** to talk to an existing Docker daemon via its socket. Accelero does not:

- run a Docker daemon or expose the Docker API
- operate AuthZ plugins
- handle plugin privilege negotiation

The following advisories describe vulnerabilities in Docker **daemon** code paths that Accelero never executes:

| Advisory | Title | Why it does not apply |
|----------|-------|----------------------|
| [GHSA-x744-4wpc-v9h2](https://github.com/advisories/GHSA-x744-4wpc-v9h2) | Moby AuthZ plugin bypass via oversized request bodies | Accelero does not run AuthZ plugins or expose the Docker API |
| [GHSA-pxq6-2prw-chj9](https://github.com/advisories/GHSA-pxq6-2prw-chj9) | Moby off-by-one in plugin privilege validation | Accelero does not negotiate plugin privileges |

Accelero inherits these advisories through the Docker SDK's shared module, but the vulnerable code paths are only reachable when operating a Docker daemon. If a fix becomes available upstream we will bump the dependency as part of routine maintenance.