#!/usr/bin/env bash

# run forever, even if we fail
while true; do
    git pull
    go run -tags release .
    sleep 1
done