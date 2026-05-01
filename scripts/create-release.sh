#!/bin/bash

set -euxo pipefail

# shellcheck disable=SC1091
. ./release-build.env
# shellcheck disable=SC1091
. ./release.env

gh release create "$RELEASE_VERSION" --title "$RELEASE_TITLE" --notes "${RELEASE_NOTE}" --latest
