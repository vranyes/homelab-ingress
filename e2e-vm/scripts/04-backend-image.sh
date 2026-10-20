#!/bin/sh
# 04-backend-image.sh — pre-pull the e2e backend image into k3s's embedded
# containerd so specs don't depend on Docker Hub at test time.
#
# Background: harness/k8s.go DeployIdenticon uses traefik/whoami. The Lima
# VM's k3s runs its own embedded containerd with no image cached, and the
# pods fail with ImagePullBackOff when the registry is unreachable from the
# VM. Pulling here (where we have retries + a loud failure) converts a
# 60s-per-spec WaitForServiceStatusIP timeout into a single provisioning
# failure with an actionable message.
#
# Must run AFTER the k3s install step in lima.yaml (needs `k3s ctr`).
set -eu

IMAGE="${BACKEND_IMAGE:-docker.io/traefik/whoami:latest}"

if ! command -v k3s >/dev/null 2>&1; then
    echo "04-backend-image: k3s not found on PATH; skipping (k3s install step may have failed)" >&2
    exit 1
fi

echo "04-backend-image: pulling $IMAGE into k3s containerd"
attempt=0
until k3s ctr images pull "$IMAGE"; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 5 ]; then
        echo "04-backend-image: ERROR failed to pull $IMAGE after 5 attempts" >&2
        echo "       check VM egress to Docker Hub; e2e specs requiring backends will fail with ImagePullBackOff" >&2
        exit 1
    fi
    echo "04-backend-image: pull failed (attempt $attempt/5), retrying in 10s..." >&2
    sleep 10
done

if k3s ctr images ls | grep -q "whoami"; then
    echo "04-backend-image: OK $IMAGE cached:"
    k3s ctr images ls | grep "whoami" || true
else
    echo "04-backend-image: ERROR pull reported success but image not listed" >&2
    k3s ctr images ls || true
    exit 1
fi
