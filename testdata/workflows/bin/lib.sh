# Shared helpers for the stub fixture scripts.

listed() {
  case ",${2}," in
  *",${1},"*) return 0 ;;
  *) return 1 ;;
  esac
}

finish() {
  node="$1"
  if listed "$node" "${WEFT_FIXTURE_FLOOD:-}"; then
    # About 10 MB, well past weft's 8 MiB per-subscriber output budget.
    awk -v n="$node" 'BEGIN { for (i = 0; i < 200000; i++) print n ": flood line to fill a pipe that nobody reads" }'
  fi
  if listed "$node" "${WEFT_FIXTURE_HANG:-}"; then
    echo "$node: hanging until canceled (WEFT_FIXTURE_HANG)"
    while :; do sleep 1; done
  fi
  if listed "$node" "${WEFT_FIXTURE_FAIL:-}"; then
    echo "$node: failing on request (WEFT_FIXTURE_FAIL)" >&2
    exit 1
  fi
}
