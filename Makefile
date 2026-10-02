.PHONY: bootstrap dependencies generate format lint test build check
TAXFIX_MAKEFILE_VERSION=1
APPLICATION_NAME=claudit

bootstrap:
	@echo "This command is currently a Noop."

dependencies:
	@echo "This command is currently a Noop."

generate:
	@echo "This command is currently a Noop."

format:
	@echo "This command is currently a Noop."

lint:
	@echo "This command is currently a Noop."

test:
	@echo "This command is currently a Noop."

IMAGE_URL=$(if $(value DOCKER_IMAGE_URL),$(value DOCKER_IMAGE_URL),$(APPLICATION_NAME))
IMAGE_TAG=$(if $(value DOCKER_IMAGE_TAG),$(value DOCKER_IMAGE_TAG),"latest")
NPM_TKN=$(if $(value NPM_TOKEN),$(value NPM_TOKEN),"")

build:
	@docker build -t $(IMAGE_URL):$(IMAGE_TAG) \
	--progress=plain \
	--build-arg=NPM_TOKEN="$(NPM_TKN)" .

check: dependencies lint test build
