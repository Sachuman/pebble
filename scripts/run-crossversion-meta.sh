#!/bin/bash

set -ex

BRANCH=$(git symbolic-ref --short HEAD)

TEMPDIR=(`mktemp -d -t crossversion-$(date +%Y-%m-%d-%H-%M-%S)-XXXXXXXXXX`)

VERSIONS=""
for branch in "$@"
do
    git checkout "$branch"
    sha=`git rev-parse --short HEAD`

    # If the branch name has a "-<suffix>", pull off the suffix. With the
    # crl-release-{XX.X} release branch naming scheme, this will extract the
    # {XX.X}.
    version="$branch"
    if [[ $branch == crl-release-* || $branch == origin/crl-release-* ]]; then
      # Extract the xy.z version name from crl-release-xy.z.
      version=`cut -d- -f3 <<< "$branch"`
    fi

    toolchain=
    if [ "$version" == "24.1" ]; then
      toolchain=go1.22.12
    fi

    echo "Building $version ($sha)"
    GOTOOLCHAIN="$toolchain" go test -c -o "$TEMPDIR/meta.$version.$sha.test" ./internal/metamorphic
    VERSIONS="$VERSIONS -version $version,$sha,$TEMPDIR/meta.$version.$sha.test"
done

# Return to whence we came.
git checkout $BRANCH

if [[ -z "${STRESS}" ]]; then
    go test ./internal/metamorphic/crossversion \
      -test.v \
      -test.timeout "${TIMEOUT:-30m}" \
      -test.run 'TestMetaCrossVersion$' \
      -seed ${SEED:-0} \
      -factor ${FACTOR:-10} \
      -artifacts ./artifacts \
      $(echo $VERSIONS)
else
    stress -p 1 go test ./internal/metamorphic/crossversion \
      -test.v \
      -test.timeout "${TIMEOUT:-30m}" \
      -test.run 'TestMetaCrossVersion$' \
      -seed ${SEED:-0} \
      -factor ${FACTOR:-10} \
      -artifacts ./artifacts \
      $(echo $VERSIONS)
fi

rm -rf $TEMPDIR
