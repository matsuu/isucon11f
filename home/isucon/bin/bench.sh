#!/bin/sh

set -ex

cwd=`dirname $0`
. ${cwd}/prepare.sh
(
  cd ~/benchmarker
  bin/benchmarker -target localhost:443 -tls | tee /result/bench.log
)
. ${cwd}/result.sh
