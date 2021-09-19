#!/bin/sh

set -ex

sudo git add /
sudo git commit -av
sudo git push origin extra-round
