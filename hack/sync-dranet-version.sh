#!/bin/bash

# Copyright The Kubernetes Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
cd "${REPO_ROOT}"

TAG="${1:-v1.1.0}"
IMAGE="registry.k8s.io/networking/dranet:${TAG}"

TARGET_FILES=(
  "install.yaml"
  "install-containerd-1.7yaml"
  "examples/dranetctl-install.yaml"
  "pkg/dranetctl/gke/components.go"
  "site/content/docs/user/gke-tpu-performance.md"
)

for f in "${TARGET_FILES[@]}"; do
  sed -E -i "s#registry\.k8s\.io/networking/dranet:[^\"'[:space:]]+#${IMAGE}#g" "${f}"
done

sed -E -i "s#^(appVersion: ).*#\\1\"${TAG}\"#" deployments/helm/dranet/Chart.yaml

sed -E -i "s#^(\s*docker tag \$\{IMAGE\} ).*#\\1\${DRANET_IMAGE}#" Makefile
sed -E -i "s#^(\s*kind load docker-image ).*( --name dra)$#\\1\${DRANET_IMAGE}\\2#" Makefile

printf "Updated DRANET image tag to %s in manifests, docs, and chart appVersion.\n" "${TAG}"
