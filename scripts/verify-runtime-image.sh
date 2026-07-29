#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: $0 IMAGE" >&2
	exit 2
fi

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temporary_root=$(mktemp -d)
container_id=
cleanup() {
	if [ -n "$container_id" ]; then
		docker rm -f "$container_id" >/dev/null 2>&1 || true
	fi
	rm -rf -- "$temporary_root"
}
trap cleanup EXIT HUP INT TERM

container_id=$(docker create "$1")
docker cp \
	"$container_id:/usr/share/licenses/llmgw/THIRD_PARTY_NOTICES.md" \
	"$temporary_root/THIRD_PARTY_NOTICES.md"
docker cp \
	"$container_id:/usr/share/licenses/llmgw/CLIProxyAPI-LICENSE" \
	"$temporary_root/CLIProxyAPI-LICENSE"

cmp "$repository_root/THIRD_PARTY_NOTICES.md" "$temporary_root/THIRD_PARTY_NOTICES.md"
cmp "$repository_root/third_party/CLIProxyAPI/LICENSE" "$temporary_root/CLIProxyAPI-LICENSE"
