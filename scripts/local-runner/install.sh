#!/bin/bash
set -euo pipefail
source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
image_file=${1:?usage: install.sh IMAGE_ID_FILE}
image_id=$(cat -- "$image_file")
[[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]]
/usr/bin/docker image inspect "$image_id" >/dev/null
for slot in 1 2; do
  state=$(systemctl --user show "orca-go-isolated-runner@$slot.service" -p ActiveState --value)
  if [[ "$state" != inactive && "$state" != failed ]]; then
    echo "Drain/stop both runner slots before updating installed code." >&2
    exit 1
  fi
done
linger=$(loginctl show-user "$USER" -p Linger --value)
if [[ "$linger" != yes ]]; then
  echo "WARNING: Linger is not enabled; user runners will not reliably persist after logout or boot." >&2
  echo 'Enable explicitly with: loginctl enable-linger "$USER"' >&2
fi
install_dir="$HOME/.local/lib/orca-go-isolated-runner"
mkdir -p -- "$install_dir" "$HOME/.config/systemd/user"
install -m 0755 "$source_dir/supervisor.py" "$install_dir/supervisor.py"
printf '%s\n' "$image_id" > "$install_dir/image-id.txt"
install -m 0644 "$source_dir/orca-go-isolated-runner@.service" "$HOME/.config/systemd/user/"
systemctl --user daemon-reload
printf 'Installed to %s; start slots explicitly after validation.\n' "$install_dir"
