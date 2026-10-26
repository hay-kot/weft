# Shared helpers for the stub fixture scripts.

listed() {
  case ",${2}," in
  *",${1},"*) return 0 ;;
  *) return 1 ;;
  esac
}

finish() {
  node="$1"
  if listed "$node" "${WEFT_FIXTURE_HANG:-}"; then
    echo "$node: hanging until canceled (WEFT_FIXTURE_HANG)"
    while :; do sleep 1; done
  fi
  if listed "$node" "${WEFT_FIXTURE_FAIL:-}"; then
    echo "$node: failing on request (WEFT_FIXTURE_FAIL)" >&2
    exit 1
  fi
}
