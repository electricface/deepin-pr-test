#!/bin/sh
set -ex

rm -rf build
mkdir build
cd build
go build -o pr-test -ldflags='-s -w' github.com/electricface/deepin-pr-test/cmd/pr-test
./pr-test --help || echo
tar -cJf pr-test.tar.xz pr-test
ls -lh pr-test.tar.xz

