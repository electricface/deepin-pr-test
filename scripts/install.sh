#!/bin/sh
set -ex
wget -O pr-test.tar.xz https://github.com/electricface/deepin-pr-test/releases/download/latest/pr-test.tar.xz
tar axf pr-test.tar.xz
sudo mv -v pr-test /usr/local/bin
