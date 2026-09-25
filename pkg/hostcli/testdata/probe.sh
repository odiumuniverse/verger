#!/bin/sh
printf 'path=%s\n' "$PATH"
printf 'argc=%s\n' "$#"
for arg in "$@"; do printf 'arg=[%s]\n' "$arg"; done
printf 'stdin='
/bin/cat
