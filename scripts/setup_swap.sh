#!/usr/bin/env bash
# Create a dedicated swap file for the translation host.
# Intended for the target Ubuntu server; requires root.
set -euo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "ERROR: run this script as root (sudo)." >&2
  exit 1
fi

SWAPFILE="${SWAPFILE:-/swapfile-translator}"
SWAPSIZE="${SWAPSIZE:-2G}"
SWAPPINESS="${SWAPPINESS:-10}"
FORCE_SWAP="${FORCE_SWAP:-0}"

swapfile_active() {
  swapon --show=NAME --noheadings | awk '{$1=$1};1' | grep -qx "$SWAPFILE"
}

required_kb=$(numfmt --from=iec "$SWAPSIZE" | awk '{print int($1/1024)}')
current_kb=$(awk '/^SwapTotal:/ {print $2}' /proc/meminfo)
if ! swapfile_active && [[ "$current_kb" -ge "$required_kb" && "$FORCE_SWAP" != "1" ]]; then
  echo "Existing swap (${current_kb} KiB) is at least ${required_kb} KiB; not adding $SWAPFILE."
  echo "Set FORCE_SWAP=1 to add the dedicated file anyway."
  install -d -m 0755 /etc/sysctl.d
  printf 'vm.swappiness=%s\n' "$SWAPPINESS" > /etc/sysctl.d/99-translator-swap.conf
  sysctl -q -w "vm.swappiness=$SWAPPINESS"
  swapon --show
  free -h
  exit 0
fi

if swapon --show=NAME --noheadings | awk '{$1=$1};1' | grep -qx "$SWAPFILE"; then
  echo "Swap file $SWAPFILE is already active."
else
  if [[ -e "$SWAPFILE" ]]; then
    echo "Reusing existing $SWAPFILE."
  else
    echo "Creating $SWAPFILE ($SWAPSIZE) ..."
    if ! fallocate -l "$SWAPSIZE" "$SWAPFILE" 2>/dev/null; then
      echo "fallocate is unavailable on this filesystem; using dd."
      rm -f "$SWAPFILE"
      # 1 MiB blocks; calculate count from the requested size.
      size_bytes=$(numfmt --from=iec "$SWAPSIZE")
      count=$(( size_bytes / 1024 / 1024 ))
      dd if=/dev/zero of="$SWAPFILE" bs=1M count="$count" status=progress
    fi
  fi
  chmod 600 "$SWAPFILE"
  chown root:root "$SWAPFILE"
  if ! swapon --show=NAME --noheadings | awk '{$1=$1};1' | grep -qx "$SWAPFILE"; then
    mkswap "$SWAPFILE"
    swapon "$SWAPFILE"
  fi
fi

# Persist across reboots exactly once.
if ! grep -qE "^[[:space:]]*${SWAPFILE}[[:space:]]" /etc/fstab; then
  printf '%s none swap sw 0 0\n' "$SWAPFILE" >> /etc/fstab
  echo "Added $SWAPFILE to /etc/fstab."
fi

# Favor RAM, but allow the model's startup peak to spill to swap.
install -d -m 0755 /etc/sysctl.d
printf 'vm.swappiness=%s\n' "$SWAPPINESS" > /etc/sysctl.d/99-translator-swap.conf
sysctl -q -w "vm.swappiness=$SWAPPINESS"

echo
swapon --show
free -h
echo "Swap setup complete."
