#!/bin/bash

ATTENUATE_URL="https://cookiebot.fly.dev/attenuate"

FLY_API_TOKEN=$(flyctl tokens create org personal -x30m | cut -d' ' -f2)
PERMISSION=$(echo $FLY_API_TOKEN | cut -d',' -f1)
AUTH=$(echo $FLY_API_TOKEN | cut -d',' -f2-)
PERMISSION=$(curl -d "$PERMISSION" $ATTENUATE_URL)
export FLY_API_TOKEN="$PERMISSION,$AUTH"
echo $FLY_API_TOKEN

LOG_LEVEL=debug flydev auth whoami