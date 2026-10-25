#!/usr/bin/env bash
set -euo pipefail
umask 077

[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]] || {
  echo "This job requires a disposable GitHub-hosted Linux runner." >&2
  exit 1
}
[[ "$(uname -s)" == Linux && -d "${RUNNER_TEMP:?}" ]]
available=$(df -B1 --output=avail "$RUNNER_TEMP" | tail -n1 | tr -d ' ')
(( available >= 1024 * 1024 * 1024 )) || {
  echo "Less than 1GiB free on runner; refusing even a bounded image." >&2
  exit 1
}
root=$(mktemp -d "$RUNNER_TEMP/kks-fs.XXXXXX")
root=$(realpath -- "$root")
image="$root/repository-volume.img"
volume="$root/mount"
loop=""

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if mountpoint -q "$volume"; then
    if [[ "$(findmnt --noheadings --raw --output SOURCE --mountpoint "$volume")" != "$loop" ]]; then
      echo "Mount ownership changed; refusing to unmount it." >&2
      exit 1
    fi
    if ! sudo umount -- "$volume"; then
      echo "Cannot unmount the test volume; leaving its image intact." >&2
      exit 1
    fi
  fi
  if [[ -n "$loop" && -e "/sys/block/${loop##*/}/loop/backing_file" ]]; then
    backing=$(cat "/sys/block/${loop##*/}/loop/backing_file")
    if [[ "$backing" != "$image" ]]; then
      echo "Loop ownership changed; refusing to detach it." >&2
      exit 1
    fi
    sudo losetup --detach "$loop" || exit 1
    sudo udevadm settle --timeout=10 || exit 1
    if [[ -e "/sys/block/${loop##*/}/loop/backing_file" ]]; then
      echo "Loop still attached; leaving image intact." >&2
      exit 1
    fi
  fi
  [[ ! -e "$image" ]] || rm -- "$image"
  [[ ! -d "$volume" ]] || rmdir -- "$volume"
  rmdir -- "$root"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -- "$volume"
# Format only this newly created regular file, never a host block device.
fallocate -l 256M "$image"
[[ -f "$image" && ! -L "$image" && "$(stat -c %s "$image")" == 268435456 ]]
mkfs.ext4 -q -F -m 0 "$image"
loop=$(sudo losetup --find --show --nooverlap "$image")
[[ "$loop" =~ ^/dev/loop[0-9]+$ ]]
[[ "$(cat "/sys/block/${loop##*/}/loop/backing_file")" == "$image" ]]
sudo mount -t ext4 -o nodev,nosuid,noexec "$loop" "$volume"
sudo chown "$(id -u):$(id -g)" "$volume"

export KKS_INTEGRATION=1
export KKS_LINUX_SAFETY=1
export KKS_TEST_IMAGE="$image"
export KKS_TEST_VOLUME="$volume"
# Go helpers re-attest the backing file, mount type and capacity before filling.
go test -count=1 -v -timeout=8m ./internal/app -run '^TestLinux'
