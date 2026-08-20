#!/bin/sh
# Demo in-container entrypoint; executes from the read-only asset mount.
echo "serving on :${LMW_PORT:-8000}"
exec sleep 3600

