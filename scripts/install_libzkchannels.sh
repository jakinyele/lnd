#!/usr/bin/env bash

LND_PATH=$1
FOUND_LND=0

if [[ $LND_PATH = "" ]]; then
   echo "Missing path to zklnd..."
fi

if [ -d "$LND_PATH/make" ]; then
   echo "Will output config to $LND_PATH/make"
   FOUND_LND=1
else
   echo "Did not specify correct path to zklnd..."
fi

git clone https://github.com/boltlabs-inc/libzkchannels.git

cd libzkchannels/

unset ZK_DEPS_INSTALL

ROOT=$(pwd)
ZK_DEPS_INSTALL=${ROOT}/deps/root

set -e
export ZK_DEPS_INSTALL
export LD_LIBRARY_PATH=${ZK_DEPS_INSTALL}/lib:${LD_LIBRARY_PATH}
export PATH=$ZK_DEPS_INSTALL/bin:$PATH

make distclean
./deps/install_packages.sh
sudo dpkg -i ./deps/emp-sh2pc/libcrypto++9v5*.deb
sudo dpkg -i ./deps/emp-sh2pc/libcrypto++-dev*.deb
make deps
redis-server --daemonize yes
redis-cli ping
curl https://build.travis-ci.org/files/rustup-init.sh -sSf | sh -s -- -y --default-toolchain stable --profile minimal
export PATH=$HOME/.cargo/bin:$PATH
rustup default stable
set +e

cargo build --release --manifest-path ./Cargo.toml

echo "export CGO_LDFLAGS=\"-L$(pwd)/target/release\"" > libzkchannels.mk
echo "export LD_LIBRARY_PATH=$LD_LIBRARY_PATH:$(pwd)/target/release:$(pwd)/deps/root/lib" >> libzkchannels.mk

if [ $FOUND_LND -eq 1 ]; then
   echo "$PWD/libzkchannels.mk"
fi
. ./libzkchannels.mk
go test -v libzkchannels.go libzkchannels_test.go
cd ..