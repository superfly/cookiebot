#!/bin/bash

export PATH="/Users/tqbf/codebase/fly/flyctl/bin:$PATH"

# SHARED_SECRET="NOT TODAY SATAN"
SERVICE_URL="https://cookiebot.fly.dev/ticket"

echo -n "$SHARED_SECRET" | base64 > "3p-ka.$$"

flyctl tokens create org personal > "org-token.$$.1"
flyctl tokens -t "$(cat org-token.$$.1)" 3p add 	\
	-l "$SERVICE_URL"				\
	--secret-file "3p-ka.$$" 			\
	> "org-token.$$.2"
flyctl tokens -t "$(cat org-token.$$.2)" 3p ticket 	\
	-l "$SERVICE_URL"				\
	> ticket.$$
flyctl tokens 3p discharge				\
	-l "$SERVICE_URL"				\
	--secret-file "3p-ka.$$"			\
	--ticket "$(cat ticket.$$)"			\
	> discharge.$$
flyctl tokens 3p add-discharge 				\
	-t "$(cat org-token.$$.2)" 			\
	--discharge "$(cat discharge.$$)"
