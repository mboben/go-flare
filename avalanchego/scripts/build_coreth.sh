#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

race=''
coreth_path=''
evm_path=''

# Directory above this script
AVALANCHE_PATH=$( cd "$( dirname "${BASH_SOURCE[0]}" )"; cd .. && pwd )

# Load the constants
source "$AVALANCHE_PATH"/scripts/constants.sh

# check if there's args defining different coreth source and build paths
if [[ $# -eq 2 ]]; then
    coreth_path=$1
    evm_path=$2
elif [[ $# -eq 0 ]]; then
    if [[ ! -d "$coreth_path" ]]; then
        go get -modcacherw "github.com/ava-labs/coreth@$coreth_version"
    fi
else
    echo "Invalid arguments to build coreth. Requires either no arguments (default) or two arguments to specify coreth directory and location to add binary."
    exit 1
fi

if [[ ! -d "$coreth_path" ]]; then
  go get "github.com/ava-labs/coreth@$coreth_version"
fi

# Build Coreth
build_args="$race"
echo "Building Coreth @ ${coreth_version} ..."
cd "$coreth_path"
go build -modcacherw -ldflags "-X github.com/ava-labs/coreth/plugin/evm.Version=$coreth_version $static_ld_flags" -o "$evm_path" "plugin/"*.go
cd "$AVALANCHE_PATH"

# Building coreth + using go get can mess with the go.mod file.
go mod tidy -compat=1.19

# Exit build successfully if the Coreth EVM binary is created successfully
if [[ -f "$evm_path" ]]; then
        echo "Coreth Build Successful"
        exit 0
else
        echo "Coreth Build Failure" >&2
        exit 1
fi
