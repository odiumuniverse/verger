#!/bin/sh
printf 'partial-out\n'
printf 'boom on stderr\n' >&2
exit 3
