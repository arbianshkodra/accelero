import docker

client = docker.from_env()


def pull_image(repository, tag):
    image_name = f"{repository}:{tag}"
    client.images.pull(repository, tag=tag)
    return image_name
