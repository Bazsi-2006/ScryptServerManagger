#!/bin/sh
# Example console-input helper ("console.input": "command").
# The console line arrives as ONE argument ($1); it is never evaluated by a
# shell on the way here. Forward it to the server however it accepts input,
# for example with an RCON client:
set -eu
exec mcrcon -H 127.0.0.1 -P 25575 -p "$(cat /opt/gameservers/example/rcon.password)" "$1"
