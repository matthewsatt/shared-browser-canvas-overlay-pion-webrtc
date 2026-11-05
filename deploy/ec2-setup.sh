#!/usr/bin/env bash
# One-time setup of a fresh Amazon Linux 2023 EC2 instance: Docker, the
# compose and buildx plugins, git. Run as ec2-user:
#
#   curl -sSLO https://<where you host this repo>/deploy/ec2-setup.sh   (or copy it over)
#   bash ec2-setup.sh
#
# Then log out and back in (for the docker group), and follow DEPLOY.md.
set -euo pipefail

if ! grep -q 'Amazon Linux' /etc/os-release || ! grep -q 'VERSION_ID="2023"' /etc/os-release; then
  echo "This script is for Amazon Linux 2023. For other systems, install Docker Engine with the compose and buildx plugins yourself." >&2
  exit 1
fi

sudo dnf install -y docker git
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"

# The compose and buildx plugins aren't in Amazon Linux's packages: take
# the latest official releases.
arch="$(uname -m)"                                  # x86_64 | aarch64
bxarch="$([ "$arch" = aarch64 ] && echo arm64 || echo amd64)"
# Fetch first, then parse: piping curl into a reader that stops early would
# trip pipefail.
latest() {
  local json
  json="$(curl -fsSL "https://api.github.com/repos/docker/$1/releases/latest")"
  printf '%s\n' "$json" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | sed -n 1p
}
compose_v="$(latest compose)"
buildx_v="$(latest buildx)"
sudo mkdir -p /usr/local/lib/docker/cli-plugins
sudo curl -fsSL "https://github.com/docker/compose/releases/download/${compose_v}/docker-compose-linux-${arch}" -o /usr/local/lib/docker/cli-plugins/docker-compose
sudo curl -fsSL "https://github.com/docker/buildx/releases/download/${buildx_v}/buildx-${buildx_v}.linux-${bxarch}" -o /usr/local/lib/docker/cli-plugins/docker-buildx
sudo chmod +x /usr/local/lib/docker/cli-plugins/docker-compose /usr/local/lib/docker/cli-plugins/docker-buildx

sudo docker compose version
sudo docker buildx version
echo
echo "Docker is ready. Log out and back in so the docker group applies, then follow DEPLOY.md."
