#!/bin/sh
set -ex
version=$(git describe --tags)
rm -rf build
mkdir build
cd build
go build -o pr-test -ldflags="-s -w -X main.VERSION=$version" github.com/electricface/deepin-pr-test/cmd/pr-test
./pr-test --help || echo
./pr-test -version
tar -cJf pr-test.tar.xz pr-test
ls -lh pr-test.tar.xz

