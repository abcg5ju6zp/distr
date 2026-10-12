#!/bin/sh

need_tty=yes
_dir=$(mktemp -d)
_script="${_dir}/distr-collect.sh"
trap 'rm -rf "$_dir"' EXIT

# Write the collect script to a temp file. When piped (curl | sh), the script
# is run as a child process with /dev/tty as stdin so that interactive prompts
# work. This is the same pattern used by rustup-init.sh.
cat > "$_script" << 'DISTR_COLLECT_EOF'
#!/bin/sh

BUNDLE_ID="{{.BundleID}}"
BASE_URL="{{.BaseURL}}"
BUNDLE_SECRET="{{.Token}}"
COLLECTION_GENERATION="{{.Generation}}"

_tmpdir=$(mktemp -d)
trap 'rm -rf "$_tmpdir"' EXIT

# --- resume state ---------------------------------------------------------
# $_resume_file holds one tab-separated line per manifest resource:
# "stableKey<TAB>status<TAB>sha256". It lets a re-run continue the same bundle
# instead of re-collecting and re-uploading resources that already reached a
# terminal outcome. A fetch failure (older server, network) leaves it empty,
# which simply means a full run; uploads stay idempotent regardless.
_resume_file="${_tmpdir}/resume-state.tsv"
: > "$_resume_file"

fetch_resume_state() {
  if curl -fsSL "${BASE_URL}/status?bundleSecret=${BUNDLE_SECRET}" \
      -o "$_resume_file" 2>/dev/null; then
    return 0
  fi
  : > "$_resume_file"
  return 1
}

# resume_status <stableKey> prints the recorded status, or nothing if unknown.
resume_status() {
  _rs_key="$1"
  while IFS="$(printf '\t')" read -r _rs_k _rs_s _rs_d; do
    if [ "$_rs_k" = "$_rs_key" ]; then
      printf '%s' "$_rs_s"
      return 0
    fi
  done < "$_resume_file"
  return 1
}

# resume_is_terminal <stableKey> succeeds when the resource already has a
# decided outcome (succeeded/skipped/oversized/failed).
resume_is_terminal() {
  case "$(resume_status "$1" 2>/dev/null)" in
    succeeded|skipped|oversized|failed) return 0 ;;
    *) return 1 ;;
  esac
}

# --- content digest -------------------------------------------------------
# sha256 is available as sha256sum (coreutils/busybox), shasum (macOS) or
# openssl. Without any of them the server still stores its own digest.
sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | sed 's/^.*= *//'
  fi
}

# --- outcome reporting ----------------------------------------------------
# report_resource <stableKey> <name> <status> [file] [reason]
# status is succeeded, skipped, oversized or failed. The stable key makes
# retries idempotent; the server verifies the sha256 digest when a file is
# attached. Returns zero when the server accepted (or had already recorded)
# the outcome.
report_resource() {
  _rp_key="$1"
  _rp_name="$2"
  _rp_status="$3"
  _rp_file="$4"
  _rp_reason="$5"
  if [ -n "$_rp_file" ] && [ ! -f "$_rp_file" ]; then
    _rp_file=""
  fi
  _rp_digest=""
  if [ -n "$_rp_file" ]; then
    _rp_digest=$(sha256_hex "$_rp_file" 2>/dev/null || true)
  fi
  _rp_err="${_tmpdir}/report_err.tmp"
  # Build curl form arguments positionally so reason text with spaces is safe.
  set -- -sS -X POST
  set -- "$@" -F "stableKey=${_rp_key}"
  set -- "$@" -F "name=${_rp_name}"
  set -- "$@" -F "status=${_rp_status}"
  if [ -n "$_rp_file" ]; then
    set -- "$@" -F "content=@${_rp_file}"
  fi
  if [ -n "$_rp_digest" ]; then
    set -- "$@" -F "sha256=${_rp_digest}"
  fi
  if [ -n "$_rp_reason" ]; then
    set -- "$@" -F "failureReason=${_rp_reason}"
  fi
  # The last line of the response body is the HTTP status code.
  _rp_resp=$(curl "$@" \
    "${BASE_URL}/resources?bundleSecret=${BUNDLE_SECRET}" \
    -w '
%{http_code}' 2>"$_rp_err") || true
  _rp_code=$(printf '%s' "$_rp_resp" | tail -n1)
  _rp_errcat=$(cat "$_rp_err" 2>/dev/null)
  rm -f "$_rp_err"
  case "$_rp_code" in
    2??)
      return 0
      ;;
    409|400)
      _rp_body=$(printf '%s' "$_rp_resp" | sed '$d' | tr '\n' ' ' | head -c 300)
      if [ -n "$_rp_body" ]; then
        echo "    Warning: ${_rp_name} was not accepted (${_rp_status}): ${_rp_body}"
      else
        echo "    Warning: ${_rp_name} was not accepted (${_rp_status})"
      fi
      ;;
    413)
      echo "    Warning: ${_rp_name} was rejected because it is too large"
      ;;
    *)
      if [ -n "$_rp_errcat" ]; then
        echo "    Warning: failed to report ${_rp_name}: ${_rp_errcat}"
      else
        echo "    Warning: failed to report ${_rp_name} (HTTP ${_rp_code})"
      fi
      ;;
  esac
  return 1
}

# upload_resource_file <stableKey> <name> <file>
upload_resource_file() {
  report_resource "$1" "$2" succeeded "$3" ""
}

# upload_resource <stableKey> <name> <content>
upload_resource() {
  _tmpfile="${_tmpdir}/upload_content.tmp"
  printf '%s' "$3" > "$_tmpfile"
  upload_resource_file "$1" "$2" "$_tmpfile"
  _u_rc=$?
  rm -f "$_tmpfile"
  return $_u_rc
}

# report_skip <stableKey> <name> <reason>
report_skip() {
  report_resource "$1" "$2" skipped "" "$3"
}
{{if .Scripts}}
# Run a custom script with the interpreter named in its shebang, falling back to sh. The
# interpreter and the timeout prefix are deliberately unquoted so that "#!/usr/bin/env bash" splits
# into command and argument. Executing the file directly instead would fail when the temp dir is
# mounted noexec.
run_custom_script() {
  _interp=$(sed -n '1s/^#![[:space:]]*//p' "$1" | tr -d '\r')
  if [ -z "$_interp" ] || ! command -v "${_interp%% *}" > /dev/null 2>&1; then
    _interp=sh
  fi
  $SCRIPT_TIMEOUT $_interp "$1"
}
{{end}}
# Parse a comma-separated list of numbers and validate each is in range 1..max.
# Outputs a validated comma set like ",1,3," for use with grep.
parse_exclude_input() {
  _input="$1"
  _max="$2"
  _input=$(printf '%s' "$_input" | tr -d ' ')
  _result=""
  if [ -z "$_input" ]; then
    return
  fi
  IFS=',' read -r _dummy <<EOF_SPLIT
$_input
EOF_SPLIT
  _remaining="$_input"
  while [ -n "$_remaining" ]; do
    _entry="${_remaining%%,*}"
    if [ "$_remaining" = "$_entry" ]; then
      _remaining=""
    else
      _remaining="${_remaining#*,}"
    fi
    case "$_entry" in
      ''|*[!0-9]*)
        echo "  Warning: ignoring invalid entry '$_entry'" >&2
        continue
        ;;
    esac
    if [ "$_entry" -lt 1 ] || [ "$_entry" -gt "$_max" ]; then
      echo "  Warning: ignoring out-of-range entry '$_entry' (valid: 1-$_max)" >&2
      continue
    fi
    _result="${_result},${_entry}"
  done
  if [ -n "$_result" ]; then
    printf '%s,' "$_result"
  fi
}

echo "=== Distr Support Bundle Collector ==="
echo "Bundle ID: ${BUNDLE_ID}"
echo "Collection generation: ${COLLECTION_GENERATION}"
echo ""

# Docker is the only part of this script that needs elevated privileges: the
# daemon socket is usually owned by root:docker. If the docker CLI is installed
# but we cannot reach the daemon because of a permission error (and we are not
# already root), collecting container data would be silently skipped. Exit early
# with a copy-pasteable command to re-run the collector with sudo instead.
if command -v docker >/dev/null 2>&1 && [ "$(id -u 2>/dev/null)" != "0" ]; then
  DOCKER_ERR=$(docker ps 2>&1 >/dev/null)
  if echo "$DOCKER_ERR" | grep -qi "permission denied"; then
    echo "Error: accessing the Docker daemon requires elevated privileges." >&2
    echo "Please re-run the collector with sudo:" >&2
    echo "" >&2
    echo "  curl -fsSL '${BASE_URL}/collect-script?bundleSecret=${BUNDLE_SECRET}' | sudo sh" >&2
    echo "" >&2
    exit 1
  fi
fi

# Load the state of the fixed manifest so a re-run after a timeout, a failed
# command or a restart continues with only the resources still missing.
if fetch_resume_state && [ -s "$_resume_file" ]; then
  echo "Found an existing collection in progress; resources already recorded will be skipped."
  echo ""
fi

# Probe the Docker daemon once, up front. Whether the daemon is reachable is
# critical diagnostic information (it may be installed but not running), so
# the status is always included with the system information below. The container
# list is captured here and reused for the log/env collection further down.
DOCKER_STATUS="Docker CLI not found in PATH"
CONTAINERS=""
if command -v docker >/dev/null 2>&1; then
  if CONTAINERS=$(docker ps -a --format "{{`{{.ID}}`}}	{{`{{.Names}}`}}	{{`{{.Status}}`}}	{{`{{.Image}}`}}" 2>/dev/null); then
    DOCKER_STATUS="daemon reachable
$(docker version 2>&1 || true)"
  else
    CONTAINERS=""
    DOCKER_STATUS="daemon unavailable
$(docker version 2>&1 || true)"
  fi
fi

# Collect system information
echo "Collecting system information..."
SYSTEM_INFO="whoami: $(whoami 2>/dev/null || echo 'unknown')
uname: $(uname -a 2>/dev/null || echo 'unknown')
hostname: $(hostname 2>/dev/null || echo 'unknown')
date: $(date 2>/dev/null || echo 'unknown')
uptime: $(uptime 2>/dev/null || echo 'unknown')

df:
$(df -h 2>/dev/null || echo 'unavailable')

memory:
$(free -h 2>/dev/null || echo 'unavailable')

docker:
$DOCKER_STATUS"
SYSINFO_KEY="system-info"

if resume_is_terminal "$SYSINFO_KEY"; then
  echo "  System information already recorded (resuming): $(resume_status "$SYSINFO_KEY")"
else
  echo ""
  echo "System information to upload:"
  echo "---"
  echo "$SYSTEM_INFO"
  echo "---"
  echo ""
  printf "Upload system information? [Y/n]: "
  read -r SYSINFO_CONFIRM
  case "$SYSINFO_CONFIRM" in
    [nN]*)
      echo "  Skipping system information upload"
      report_skip "$SYSINFO_KEY" "system-info" "skipped by user"
      ;;
    *)
      if upload_resource "$SYSINFO_KEY" "system-info" "$SYSTEM_INFO"; then
        echo "  Uploaded system information"
      fi
      ;;
  esac
fi

# Detect Docker containers and build included container list (reusing the list
# captured during the Docker probe above)
echo ""
echo "Detecting Docker containers..."

CONTAINER_COUNT=0
INCLUDED_CONTAINERS=""
if [ -z "$CONTAINERS" ]; then
  echo "  No Docker containers found (see Docker status in system information)"
else
  echo ""
  echo "Available containers:"
  echo "---"
  IDX=1
  while IFS="$(printf '\t')" read -r CID CNAME CSTATUS CIMAGE; do
    printf "  [%d] %s (%s) - %s\n" "$IDX" "$CNAME" "$CSTATUS" "$CIMAGE"
    IDX=$((IDX + 1))
  done <<EOF_CONTAINERS
$CONTAINERS
EOF_CONTAINERS
  CONTAINER_COUNT=$((IDX - 1))
  echo ""
  echo "Enter container numbers to EXCLUDE (comma-separated), or press Enter to include all:"
  read -r EXCLUDE_INPUT
  EXCLUDE_SET=$(parse_exclude_input "$EXCLUDE_INPUT" "$CONTAINER_COUNT")

  # Build the list of included containers (ID<tab>Name per line)
  IDX=1
  while IFS="$(printf '\t')" read -r CID CNAME _CSTATUS _CIMAGE; do
    if [ -z "$EXCLUDE_SET" ] || ! echo "$EXCLUDE_SET" | grep -q ",$IDX,"; then
      INCLUDED_CONTAINERS="${INCLUDED_CONTAINERS}${CID}	${CNAME}
"
    fi
    IDX=$((IDX + 1))
  done <<EOF_CONTAINERS
$CONTAINERS
EOF_CONTAINERS
fi

# Collect environment variables from host and containers
echo ""
echo "Collecting environment variables..."
ENV_GROUP_COUNT=0

# Collect host environment variables
ENV_GROUP_COUNT=$((ENV_GROUP_COUNT + 1))
HOST_ENV=""
{{- range .EnvVars}}
_val=$(printenv "{{.Name}}" 2>/dev/null || true)
{{- if .Redacted}}
if [ -n "$_val" ]; then _val="[REDACTED]"; fi
{{- end}}
HOST_ENV="${HOST_ENV}{{.Name}}=${_val}
"
{{- end}}
printf '%s' "$HOST_ENV" > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.txt"
printf '%s' "Host" > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.name"
printf '%s' "host-environment-variables" > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.resource"
: > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.fail"

# Collect container environment variables
if [ -n "$INCLUDED_CONTAINERS" ]; then
  while IFS="$(printf '\t')" read -r CID CNAME; do
    [ -z "$CID" ] && continue
    ENV_GROUP_COUNT=$((ENV_GROUP_COUNT + 1))
    CONTAINER_ENV=$(docker exec "$CID" env 2>/dev/null) || \
      CONTAINER_ENV=$(docker inspect --format '{{`{{range .Config.Env}}{{println .}}{{end}}`}}' "$CID" 2>/dev/null) || true
    if [ -n "$CONTAINER_ENV" ]; then
      FILTERED_ENV=""
{{- range .EnvVars}}
      _val=$(echo "$CONTAINER_ENV" | grep "^{{.Name}}=" | head -1 | cut -d= -f2-)
{{- if .Redacted}}
      if [ -n "$_val" ]; then _val="[REDACTED]"; fi
{{- end}}
      FILTERED_ENV="${FILTERED_ENV}{{.Name}}=${_val}
"
{{- end}}
      printf '%s' "$FILTERED_ENV" > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.txt"
      : > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.fail"
    else
      printf '%s' "Error: could not collect container environment variables" > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.txt"
      printf '%s' "could not collect container environment variables" > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.fail"
    fi
    printf '%s' "$CNAME" > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.name"
    printf '%s' "${CNAME}-container-env" > "${_tmpdir}/envgroup_${ENV_GROUP_COUNT}.resource"
  done <<EOF_INCLUDED
$INCLUDED_CONTAINERS
EOF_INCLUDED
fi

# Display environment variable groups and let user select
if [ "$ENV_GROUP_COUNT" -gt 0 ]; then
  echo ""
  echo "Environment variables to upload:"
  echo "---"
  _g=1
  while [ "$_g" -le "$ENV_GROUP_COUNT" ]; do
    _gname=$(cat "${_tmpdir}/envgroup_${_g}.name")
    printf "  [%d] %s\n" "$_g" "$_gname"
    while IFS= read -r _line || [ -n "$_line" ]; do
      printf "      %s\n" "$_line"
    done < "${_tmpdir}/envgroup_${_g}.txt"
    echo ""
    _g=$((_g + 1))
  done

  echo "Enter group numbers to EXCLUDE from upload (comma-separated), or press Enter to include all:"
  read -r ENV_EXCLUDE_INPUT
  ENV_EXCLUDE_SET=$(parse_exclude_input "$ENV_EXCLUDE_INPUT" "$ENV_GROUP_COUNT")

  # Report an outcome for every env group so each required/optional resource ends in a terminal state.
  _g=1
  while [ "$_g" -le "$ENV_GROUP_COUNT" ]; do
    _gname=$(cat "${_tmpdir}/envgroup_${_g}.name")
    _gresource=$(cat "${_tmpdir}/envgroup_${_g}.resource")
    _gfile="${_tmpdir}/envgroup_${_g}.fail"
    if resume_is_terminal "$_gresource"; then
      echo "  $_gname already recorded (resuming): $(resume_status "$_gresource")"
    elif [ -n "$ENV_EXCLUDE_SET" ] && echo "$ENV_EXCLUDE_SET" | grep -q ",$_g,"; then
      echo "  Skipping env vars for $_gname"
      report_skip "$_gresource" "$_gresource" "skipped by user"
    elif [ -s "$_gfile" ]; then
      _greason=$(cat "$_gfile")
      if report_resource "$_gresource" "$_gresource" failed "${_tmpdir}/envgroup_${_g}.txt" "$_greason"; then
        echo "  Recorded collection failure for $_gname"
      fi
    elif [ -s "${_tmpdir}/envgroup_${_g}.txt" ]; then
      if upload_resource_file "$_gresource" "$_gresource" "${_tmpdir}/envgroup_${_g}.txt"; then
        echo "  Uploaded env vars for $_gname"
      fi
    else
      report_skip "$_gresource" "$_gresource" "no environment variables collected"
    fi
    _g=$((_g + 1))
  done
fi

# Collect and upload container logs
if [ -n "$INCLUDED_CONTAINERS" ]; then
  echo ""
  echo "Collecting and uploading container logs..."
  _lograw="${_tmpdir}/container_logs.raw"
  _logout="${_tmpdir}/container_logs.out"
  while IFS="$(printf '\t')" read -r CID CNAME; do
    [ -z "$CID" ] && continue
    _lkey="${CNAME}-container-logs"
    if resume_is_terminal "$_lkey"; then
      echo "  Logs for $CNAME already recorded (resuming): $(resume_status "$_lkey")"
      continue
    fi
    docker logs --tail {{.LogTail}} "$CID" > "$_lograw" 2>&1 || true
    if [ -s "$_lograw" ]; then
      # Logs are cut off at the front, unlike script output: the most recent lines matter most.
      if [ "$(wc -c < "$_lograw")" -gt {{.ResourceMaxBytes}} ]; then
        printf '%s\n' "--- distr: earlier output truncated at {{.ResourceMaxBytes}} bytes ---" > "$_logout"
        tail -c {{.ResourceMaxBytes}} "$_lograw" >> "$_logout"
        _lognote=" (truncated to {{.ResourceMaxBytes}} bytes)"
      else
        cat "$_lograw" > "$_logout"
        _lognote=""
      fi
      if upload_resource_file "$_lkey" "$_lkey" "$_logout"; then
        echo "  Uploaded logs for $CNAME${_lognote}"
      fi
    else
      echo "  No logs available for $CNAME"
      report_skip "$_lkey" "$_lkey" "no logs available"
    fi
    rm -f "$_lograw" "$_logout"
  done <<EOF_INCLUDED
$INCLUDED_CONTAINERS
EOF_INCLUDED
fi

{{- if .Scripts}}

# Run the custom scripts configured by the vendor and collect their output. Their names,
# descriptions and bodies are embedded base64-encoded so that nothing in them can terminate the
# heredoc above, which would make the rest of this file run as the outer script.
echo ""
echo "Preparing custom scripts..."
SCRIPT_COUNT=0
SCRIPT_TIMEOUT=""
if ! command -v base64 > /dev/null 2>&1; then
  echo "  Warning: base64 is not available, skipping custom scripts"
else
  mkdir -p "${_tmpdir}/scripts"
{{- if .ScriptTimeoutSeconds}}
  # timeout is in coreutils and busybox, but not on every host (macOS ships without it).
  if command -v timeout > /dev/null 2>&1; then
    SCRIPT_TIMEOUT="timeout -k 5 {{.ScriptTimeoutSeconds}}"
  else
    echo "  Warning: timeout is not available, scripts run without a time limit"
  fi
{{- end}}
{{- range .Scripts}}
  SCRIPT_COUNT=$((SCRIPT_COUNT + 1))
  printf '%s' '{{.ID}}' > "${_tmpdir}/scripts/${SCRIPT_COUNT}.id"
  printf '%s' '{{.NameBase64}}' | base64 -d > "${_tmpdir}/scripts/${SCRIPT_COUNT}.name"
  printf '%s' '{{.DescriptionBase64}}' | base64 -d > "${_tmpdir}/scripts/${SCRIPT_COUNT}.desc"
  printf '%s' '{{.ContentBase64}}' | base64 -d > "${_tmpdir}/scripts/${SCRIPT_COUNT}.sh"
{{- end}}
fi

if [ "$SCRIPT_COUNT" -gt 0 ]; then
  echo ""
  echo "Your vendor provided the following scripts to run on this host:"
  echo "---"
  _s=1
  while [ "$_s" -le "$SCRIPT_COUNT" ]; do
    printf "  [%d] %s\n" "$_s" "$(cat "${_tmpdir}/scripts/${_s}.name")"
    _sdesc=$(cat "${_tmpdir}/scripts/${_s}.desc")
    if [ -n "$_sdesc" ]; then
      printf "      %s\n" "$_sdesc"
    fi
    echo ""
    while IFS= read -r _line || [ -n "$_line" ]; do
      printf "      %s\n" "$_line"
    done < "${_tmpdir}/scripts/${_s}.sh"
    echo ""
    _s=$((_s + 1))
  done

  echo "Enter script numbers to EXCLUDE from running (comma-separated), or press Enter to run all:"
  read -r SCRIPT_EXCLUDE_INPUT
  SCRIPT_EXCLUDE_SET=$(parse_exclude_input "$SCRIPT_EXCLUDE_INPUT" "$SCRIPT_COUNT")

  echo ""
  SCRIPT_OUTPUT_COUNT=0
  _s=1
  while [ "$_s" -le "$SCRIPT_COUNT" ]; do
    _sname=$(cat "${_tmpdir}/scripts/${_s}.name")
    _sbase="${_tmpdir}/scripts/${_s}"
    _skey="custom-script:$(cat "${_sbase}.id")"
    if resume_is_terminal "$_skey"; then
      echo "  Skipping ${_sname} (already recorded: $(resume_status "$_skey"))"
    elif [ -n "$SCRIPT_EXCLUDE_SET" ] && echo "$SCRIPT_EXCLUDE_SET" | grep -q ",$_s,"; then
      echo "  Skipping ${_sname}"
      report_skip "$_skey" "$_sname" "skipped by user"
    else
      echo "  Running ${_sname}..."
      # Custom scripts must not consume the stdin the collector reads its prompts from.
      run_custom_script "${_sbase}.sh" > "${_sbase}.raw" 2> "${_sbase}.err" < /dev/null
      _scode=$?
      head -c {{.ResourceMaxBytes}} "${_sbase}.raw" > "${_sbase}.out"
      # stdout and stderr share the per-resource budget, so one script cannot contribute twice the cap.
      _sbudget=$(({{.ResourceMaxBytes}} - $(wc -c < "${_sbase}.out")))
      if [ "$(wc -c < "${_sbase}.raw")" -gt {{.ResourceMaxBytes}} ]; then
        printf '\n--- distr: output truncated at %s bytes ---\n' "{{.ResourceMaxBytes}}" >> "${_sbase}.out"
      fi
      # coreutils reports a timeout as 124, and as 128+n when the script had to be signalled.
      _sreason=""
      if [ -n "$SCRIPT_TIMEOUT" ] && { [ "$_scode" -eq 124 ] || [ "$_scode" -eq 137 ] || [ "$_scode" -eq 143 ]; }; then
        _sreason="script timed out after {{.ScriptTimeoutSeconds}} seconds"
        printf '\n--- distr: script timed out after %ss ---\n' \
          "{{.ScriptTimeoutSeconds}}" >> "${_sbase}.out"
      elif [ "$_scode" -ne 0 ]; then
        _sreason="script exited with code ${_scode}"
        printf '\n--- distr: script exited with code %s ---\n' "$_scode" >> "${_sbase}.out"
      fi
      if [ -s "${_sbase}.err" ]; then
        printf '\n--- distr: stderr ---\n' >> "${_sbase}.out"
        if [ "$_sbudget" -gt 0 ]; then
          head -c "$_sbudget" "${_sbase}.err" >> "${_sbase}.out"
        fi
        if [ "$(wc -c < "${_sbase}.err")" -gt "$_sbudget" ]; then
          printf '\n--- distr: stderr truncated at %s bytes of total output ---\n' \
            "{{.ResourceMaxBytes}}" >> "${_sbase}.out"
        fi
      fi
      printf '%s' "$_sreason" > "${_sbase}.reason"
      SCRIPT_OUTPUT_COUNT=$((SCRIPT_OUTPUT_COUNT + 1))
    fi
    _s=$((_s + 1))
  done

  if [ "$SCRIPT_OUTPUT_COUNT" -gt 0 ]; then
    echo ""
    echo "Script output to upload:"
    echo "---"
    _s=1
    while [ "$_s" -le "$SCRIPT_COUNT" ]; do
      _sbase="${_tmpdir}/scripts/${_s}"
      if [ -f "${_sbase}.out" ]; then
        printf "  %s\n" "$(cat "${_sbase}.name")"
        _slines=$(wc -l < "${_sbase}.out")
        head -n 30 "${_sbase}.out" | while IFS= read -r _line || [ -n "$_line" ]; do
          printf "      %s\n" "$_line"
        done
        if [ "$_slines" -gt 30 ]; then
          printf "      ... (%s more lines)\n" "$((_slines - 30))"
        fi
        echo ""
      fi
      _s=$((_s + 1))
    done

    printf "Upload script output? [Y/n]: "
    read -r SCRIPT_CONFIRM
    case "$SCRIPT_CONFIRM" in
      [nN]*)
        echo "  Skipping script output upload"
        _s=1
        while [ "$_s" -le "$SCRIPT_COUNT" ]; do
          _sbase="${_tmpdir}/scripts/${_s}"
          if [ -f "${_sbase}.out" ]; then
            report_skip "custom-script:$(cat "${_sbase}.id")" "$(cat "${_sbase}.name")" \
              "upload declined by user"
          fi
          _s=$((_s + 1))
        done
        ;;
      *)
        _s=1
        while [ "$_s" -le "$SCRIPT_COUNT" ]; do
          _sbase="${_tmpdir}/scripts/${_s}"
          if [ -f "${_sbase}.out" ]; then
            _sname=$(cat "${_sbase}.name")
            _skey="custom-script:$(cat "${_sbase}.id")"
            _sreason=$(cat "${_sbase}.reason" 2>/dev/null || true)
            if [ ! -s "${_sbase}.out" ]; then
              report_skip "$_skey" "$_sname" "script produced no output"
            elif [ -n "$_sreason" ]; then
              if report_resource "$_skey" "$_sname" failed "${_sbase}.out" "$_sreason"; then
                echo "  Recorded failure of ${_sname}: ${_sreason}"
              fi
            else
              if upload_resource_file "$_skey" "$_sname" "${_sbase}.out"; then
                echo "  Uploaded output of ${_sname}"
              fi
            fi
          fi
          _s=$((_s + 1))
        done
        ;;
    esac
  fi
fi
{{- end}}

# Finalize support bundle. finalize is idempotent: if the response of the first
# call was lost, re-posting returns the original result.
echo ""
echo "Finalizing support bundle..."
_fin_resp="${_tmpdir}/finalize.tmp"
_fin_code=$(curl -sS -X POST \
  "${BASE_URL}/finalize?bundleSecret=${BUNDLE_SECRET}" \
  -o "$_fin_resp" -w '%{http_code}' 2>/dev/null || true)
case "$_fin_code" in
  2??)
    echo "Support bundle finalized successfully."
    ;;
  409)
    echo "Warning: support bundle is not complete yet:"
    head -c 600 "$_fin_resp" 2>/dev/null
    echo ""
    echo "Re-run the collector to finish the missing resources:"
    echo "  curl -fsSL '${BASE_URL}/collect-script?bundleSecret=${BUNDLE_SECRET}' | sh"
    ;;
  *)
    echo "Warning: failed to finalize support bundle (HTTP ${_fin_code})."
    echo "Re-run the collector to retry finalization:"
    echo "  curl -fsSL '${BASE_URL}/collect-script?bundleSecret=${BUNDLE_SECRET}' | sh"
    ;;
esac
rm -f "$_fin_resp"
echo ""
echo "Support bundle collection complete!"
echo "Bundle ID: ${BUNDLE_ID}"
DISTR_COLLECT_EOF

chmod u+x "$_script"

if [ "$need_tty" = "yes" ] && [ ! -t 0 ]; then
  # The script was piped into sh (e.g., curl | sh) and doesn't have stdin to
  # pass to the child process. Explicitly connect /dev/tty to stdin.
  if [ ! -t 1 ]; then
    echo "Unable to run interactively." >&2
    exit 1
  fi
  sh "$_script" < /dev/tty
else
  sh "$_script"
fi
