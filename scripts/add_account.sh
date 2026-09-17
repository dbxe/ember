#!/bin/sh
set -eu

replace=0
if [ "${1:-}" = "--replace" ]; then
    replace=1
    shift
fi

name=${1:-}
case "$name" in
    ""|*[!A-Za-z0-9._-]*)
        echo "account name must contain only letters, numbers, dots, underscores, and hyphens" >&2
        exit 2
        ;;
esac

if ! command -v codex >/dev/null 2>&1; then
    echo "codex is not on PATH" >&2
    exit 1
fi

if pgrep -x ember >/dev/null 2>&1; then
    echo "quit Ember before adding an account" >&2
    exit 1
fi

codex_dir=$HOME/.codex
accounts_dir=$codex_dir/accounts
auth_path=$codex_dir/auth.json
current_path=$codex_dir/current
target_path=$accounts_dir/$name.json

if [ ! -f "$auth_path" ]; then
    echo "no current Codex login found at $auth_path" >&2
    exit 1
fi
if [ -e "$target_path" ] && [ "$replace" -eq 0 ]; then
    echo "account '$name' already exists" >&2
    exit 1
fi

mkdir -p "$accounts_dir"
chmod 700 "$accounts_dir"

previous_name=
if [ -f "$current_path" ]; then
    previous_name=$(sed -n '1p' "$current_path")
fi
case "$previous_name" in
    "" ) ;;
    *[!A-Za-z0-9._-]*)
        echo "current Ember account name is invalid: $previous_name" >&2
        exit 1
        ;;
esac
if [ -n "$previous_name" ]; then
    previous_tmp=$accounts_dir/.$previous_name.json.tmp.$$
    cp "$auth_path" "$previous_tmp"
    chmod 600 "$previous_tmp"
    mv "$previous_tmp" "$accounts_dir/$previous_name.json"
fi

backup_dir=$(mktemp -d "${TMPDIR:-/tmp}/ember-add-account.XXXXXX")
chmod 700 "$backup_dir"
cp "$auth_path" "$backup_dir/auth.json"
chmod 600 "$backup_dir/auth.json"
restore_required=1

restore_previous_login() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$restore_required" -eq 1 ]; then
        cp "$backup_dir/auth.json" "$auth_path"
        chmod 600 "$auth_path"
        if [ -n "$previous_name" ]; then
            current_tmp=$codex_dir/.current.tmp.$$
            printf '%s\n' "$previous_name" > "$current_tmp"
            chmod 600 "$current_tmp"
            mv "$current_tmp" "$current_path"
        fi
        echo "login was not completed; restored the previous Codex login" >&2
    fi
    rm -rf "$backup_dir"
    exit "$status"
}
trap restore_previous_login EXIT
trap 'exit 1' HUP INT TERM

if [ "$replace" -eq 1 ]; then
    echo "Codex will now sign out. Complete the next login with the account to reauthenticate as '$name'."
else
    echo "Codex will now sign out. Complete the next login with the account to save as '$name'."
fi
codex logout
codex login

if [ ! -f "$auth_path" ]; then
    echo "codex login completed without creating $auth_path" >&2
    exit 1
fi

target_tmp=$accounts_dir/.$name.json.tmp.$$
cp "$auth_path" "$target_tmp"
chmod 600 "$target_tmp"
mv "$target_tmp" "$target_path"

current_tmp=$codex_dir/.current.tmp.$$
printf '%s\n' "$name" > "$current_tmp"
chmod 600 "$current_tmp"
mv "$current_tmp" "$current_path"

restore_required=0
if [ "$replace" -eq 1 ]; then
    echo "reauthenticated and activated Ember account '$name'"
else
    echo "saved and activated Ember account '$name'"
fi
