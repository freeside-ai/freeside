package projectimage

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const prepareScript = `#!/bin/sh
set -eu
if ! cmp -s package.json /opt/freeside/project-seed/package.json ||
	! cmp -s package-lock.json /opt/freeside/project-seed/package-lock.json; then
	echo "project dependency manifests differ from the baked image inputs" >&2
	exit 42
fi
if [ -e npm-shrinkwrap.json ] || [ -e .npmrc ]; then
	echo "project contributes unsupported npm lock or configuration inputs" >&2
	exit 42
fi
export NPM_CONFIG_GLOBALCONFIG=/usr/local/etc/npmrc
export NPM_CONFIG_USERCONFIG=/dev/null
exec npm ci --ignore-scripts
`

func renderContainerfile(request Request, recipeDigest domain.Digest) string {
	// Every interpolated value has already passed validateRequest's restricted
	// grammar. The dependency manifests and exact recipe bytes enter through
	// COPY, never through instruction text.
	return fmt.Sprintf(`# Generated runtime artifact for %s at %s.
# It deliberately contributes no ENV, WORKDIR, ENTRYPOINT, CMD, USER, or
# VOLUME: project images pass the same ward realized-shape gate as agent bases.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

ARG BASE_DIGEST
ARG PROJECT_REPOSITORY
ARG PROJECT_REPOSITORY_ID
ARG PROJECT_COMMIT
ARG PROJECT_RECIPE_DIGEST

RUN set -eux; \
	mkdir -p /usr/local/etc /usr/local/share/freeside /opt/freeside/npm-cache /opt/freeside/project-seed; \
	printf '%%s\n' \
		'cache=%s' \
		'prefer-offline=true' \
		'audit=false' \
		'fund=false' \
		'update-notifier=false' > /usr/local/etc/npmrc; \
	test "$(npm config get cache)" = "%s"

COPY package.json package-lock.json /opt/freeside/project-seed/
COPY recipe.json %s
COPY prepare %s

# Cache warming executes no repository or dependency lifecycle scripts. The
# fresh verification workspace hydrates the same way: the image-owned
# preparation helper runs npm ci --ignore-scripts under --network none using
# only these cached tarballs.
RUN set -eux; \
	cd /opt/freeside/project-seed; \
	NPM_CONFIG_GLOBALCONFIG=/usr/local/etc/npmrc \
	NPM_CONFIG_USERCONFIG=/dev/null \
		npm ci --ignore-scripts; \
	rm -rf node_modules

LABEL org.opencontainers.image.title="freeside-project-image" \
	ai.freeside.base.digest="${BASE_DIGEST}" \
	ai.freeside.project.repository="${PROJECT_REPOSITORY}" \
	ai.freeside.project.repository-id="${PROJECT_REPOSITORY_ID}" \
	ai.freeside.project.commit="${PROJECT_COMMIT}" \
	ai.freeside.project.recipe-digest="${PROJECT_RECIPE_DIGEST}"
`, request.Repository, request.CommitSHA, NPMCachePath, NPMCachePath, ward.ProjectRecipePath, PreparationPath) +
		"# Bound recipe digest: " + string(recipeDigest) + "\n"
}
