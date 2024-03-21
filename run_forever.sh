#!/usr/bin/env bash

# run forever, even if we fail
while true; do
    git pull
    go run .
    sleep 1
done