#!/usr/bin/env bash
# Wires a local image registry into the kind box, so a caller (any
# repository, not only truvity/policy) can push a snapshot image on the
# runner and have the cluster pull the SAME tag. Follows kind's own
# documented pattern exactly:
# https://kind.sigs.k8s.io/docs/user/local-registry/
#
# Runs AFTER the cluster exists (policy's up.sh created it) rather than
# being folded into cluster creation, and deliberately does not patch
# containerd's config: kind >= v0.27 already enables the
# `/etc/containerd/certs.d` config_path on every node by default (
# https://github.com/kubernetes-sigs/kind/issues/2875), and the version
# this action pins (v0.33.0) postdates that by a long way. That is what
# makes this addable without policy's cluster.yaml carrying anything
# about a registry it does not itself need — the box stays exactly what
# policy owns.
#
# IDEMPOTENT AGAINST A BOX THAT ALREADY BRINGS ITS OWN, deliberately: this
# action's SNAPSHOT_REGISTRY contract (localhost:5001, always) has to hold
# for whichever policy-version a caller pins, and truvity/policy's own
# hack/kind/ has carried a registry on that exact port before (v0.8.0's
# up.sh, kind-registry on 127.0.0.1:5001) and dropped it since (master, as
# read while building this action). Binding the SAME host port a second
# time would fail outright rather than quietly duplicate anything, so the
# first move here is to ask whether something already answers on it.
set -euo pipefail

kubeconfig=${1:?kubeconfig path required}

reg_name=cluster-action-registry
reg_port=5001

if docker ps --format '{{.Ports}}' | grep -q ":${reg_port}->"; then
  echo "something already publishes localhost:${reg_port} (likely the box's own registry) — leaving it as is"
  exit 0
fi

if [ "$(docker inspect -f '{{.State.Running}}' "$reg_name" 2>/dev/null || true)" != true ]; then
  docker run \
    -d --restart=always -p "127.0.0.1:${reg_port}:5000" --network bridge --name "$reg_name" \
    registry@sha256:852b3e4d378c426dda6b318fe9d9bfe8e92a0eccb9926671ec3d3ea17a196696
fi

# The cluster's own name, whatever it is: reading it off the kubeconfig
# rather than assuming "policy" (today's versions.env value) keeps this
# working across whatever policy names its box in a later release.
cluster_name=$(KUBECONFIG="$kubeconfig" kubectl config current-context | sed 's/^kind-//')

# localhost resolves inside a node's network namespace, not the host's —
# so nodes are told that "localhost:5001" means "ask the registry
# container by its Docker DNS name" instead.
registry_dir="/etc/containerd/certs.d/localhost:${reg_port}"
for node in $(kind get nodes --name "$cluster_name"); do
  docker exec "$node" mkdir -p "$registry_dir"
  printf '[host."http://%s:5000"]\n' "$reg_name" |
    docker exec -i "$node" cp /dev/stdin "$registry_dir/hosts.toml"
done

if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "$reg_name")" = null ]; then
  docker network connect kind "$reg_name"
fi

KUBECONFIG="$kubeconfig" kubectl apply -f - <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${reg_port}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
YAML

echo "local registry ready: push to localhost:${reg_port} from the runner, the cluster resolves the same tag"
