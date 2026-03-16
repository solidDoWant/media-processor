#!/usr/bin/env bash
FLAKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"                                                                                                                                                          
exec /nix/var/nix/profiles/default/bin/nix develop "$FLAKE_DIR" --command go "$@" 
