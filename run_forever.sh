#!/usr/bin/env bash

# run forever, even if we fail
while true; do
    git pull
    go build -tags release -o mire
    ./mire
    sleep 1
done